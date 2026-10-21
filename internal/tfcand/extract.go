// Package tfcand screens Go test files for test-failure (TF) candidates used by the structural builder.
//
// Screening uses file-local syntax and parameter bindings, without loading dependencies
// or inferring aliases. Excluded occurrences keep their reasons so they cannot be
// mistaken for checked negatives. Spans refer to the original source bytes.
package tfcand

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Schema identifies TF screening rows.
const Schema = "tf-call-v1"

// ParserMode keeps comments for generated-file detection and documentation spans.
// Object resolution is needed to distinguish testing parameters from shadowing locals.
const ParserMode = parser.ParseComments | parser.AllErrors

var targets = map[string]bool{"Error": true, "Errorf": true, "Fatal": true, "Fatalf": true}

// Target reports whether method is one of the four TF methods.
func Target(method string) bool { return targets[method] }

// Slice is a half-open original byte range with its first line and digest.
type Slice struct {
	Start  int    `json:"start"`
	End    int    `json:"end"`
	Line   int    `json:"line"`
	SHA256 string `json:"sha256"`
}

// FileRow records file coverage. Package is set only after parsing.
// Exclusion counts keep unsupported calls visible.
type FileRow struct {
	Schema         string         `json:"schema"`
	Kind           string         `json:"kind"`
	Path           string         `json:"path"`
	SHA256         *string        `json:"sha256"`
	Bytes          int            `json:"bytes"`
	Status         string         `json:"status"`
	Reason         *string        `json:"reason"`
	Generated      bool           `json:"generated"`
	Package        *string        `json:"package"`
	TestingImports []string       `json:"testing_imports"`
	DotImport      bool           `json:"dot_import"`
	BlankImport    bool           `json:"blank_import"`
	Calls          int            `json:"calls"`
	Selectors      int            `json:"selectors"`
	Admitted       int            `json:"admitted"`
	Exclusions     map[string]int `json:"exclusions"`
}

// Literal is one function literal enclosing an admitted target.
type Literal struct {
	Position string `json:"position"`
	Span     Slice  `json:"span"`
}

// Owner is the declaration or literal that declares a testing binding.
type Owner struct {
	Kind string `json:"kind"`
	Span Slice  `json:"span"`
}

// Binding is the parameter an admitted receiver resolves to.
type Binding struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Field Slice  `json:"field"`
	Owner Owner  `json:"owner"`
}

// RunName is the enclosing subtest name of an admitted target.
type RunName struct {
	Kind  string  `json:"kind"`
	Value *string `json:"value"`
	Span  *Slice  `json:"span"`
}

// Declaration is the named function or method that holds an admitted target. Its span starts at the attached
// documentation when there is one.
type Declaration struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Span Slice  `json:"span"`
}

// CallRow records an examined occurrence. Admitted-only fields are nil for exclusions.
// Target spans the whole expression statement when possible, otherwise the call or
// selector. Sel is the method identifier offset used to join the independent sentry.
type CallRow struct {
	Schema           string       `json:"schema"`
	Kind             string       `json:"kind"`
	Path             string       `json:"path"`
	FileSHA256       string       `json:"file_sha256"`
	Unit             string       `json:"unit"`
	Method           string       `json:"method"`
	Receiver         *string      `json:"receiver"`
	Statement        string       `json:"statement"`
	Admitted         bool         `json:"admitted"`
	Reason           *string      `json:"reason"`
	Target           Slice        `json:"target"`
	Declaration      *Declaration `json:"declaration"`
	Binding          *Binding     `json:"binding"`
	Literals         []Literal    `json:"literals"`
	Callback         *bool        `json:"callback"`
	CallerContext    *string      `json:"caller_context"`
	ContextReasons   []string     `json:"context_reasons"`
	EnclosingRunName *RunName     `json:"enclosing_run_name"`
	Sel              int          `json:"-"`
}

// param is one parameter declaration. kind is T, B, TB, dot, other, receiver or result.
type param struct {
	owner ast.Node
	field *ast.Field
	kind  string
}

type extractor struct {
	src    []byte
	fset   *token.FileSet
	path   string
	sha    string
	names  map[string]bool // local names of ordinary testing imports
	dot    bool
	params map[*ast.Field]param
	file   *FileRow
	rows   []CallRow
	occs   []Occurrence // admitted rows, Row indexing rows before sorting
}

func (e *extractor) imports(f *ast.File) {
	for _, spec := range f.Imports {
		if p, err := strconv.Unquote(spec.Path.Value); err != nil || p != "testing" {
			continue
		}
		switch {
		case spec.Name == nil:
			e.names["testing"] = true
		case spec.Name.Name == "_":
			e.file.BlankImport = true
		case spec.Name.Name == ".":
			e.file.DotImport, e.dot = true, true
		default:
			e.names[spec.Name.Name] = true
		}
	}
	for name := range e.names {
		e.file.TestingImports = append(e.file.TestingImports, name)
	}
	sort.Strings(e.file.TestingImports)
}

func (e *extractor) declare(f *ast.File) {
	add := func(owner ast.Node, list *ast.FieldList, kind string) {
		if list == nil {
			return
		}
		for _, field := range list.List {
			k := kind
			if k == "" {
				k = e.typeKind(field.Type)
			}
			e.params[field] = param{owner: owner, field: field, kind: k}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			add(x, x.Recv, "receiver")
			add(x, x.Type.Params, "")
			add(x, x.Type.Results, "result")
		case *ast.FuncLit:
			add(x, x.Type.Params, "")
			add(x, x.Type.Results, "result")
		}
		return true
	})
}

func (e *extractor) typeKind(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		switch x := t.X.(type) {
		case *ast.SelectorExpr:
			if e.testingPackage(x.X) && (x.Sel.Name == "T" || x.Sel.Name == "B") {
				return x.Sel.Name
			}
		case *ast.Ident:
			if e.dot && x.Obj == nil && (x.Name == "T" || x.Name == "B") {
				return "dot"
			}
		}
	case *ast.SelectorExpr:
		if e.testingPackage(t.X) && t.Sel.Name == "TB" {
			return "TB"
		}
	case *ast.Ident:
		if e.dot && t.Obj == nil && t.Name == "TB" {
			return "dot"
		}
	}
	return "other"
}

// testingPackage reports whether x names an ordinary testing import that no file-local declaration hides.
func (e *extractor) testingPackage(x ast.Expr) bool {
	id, ok := x.(*ast.Ident)
	return ok && id.Obj == nil && e.names[id.Name]
}

func (e *extractor) paramOf(id *ast.Ident) *param {
	if id.Obj == nil || id.Obj.Kind != ast.Var {
		return nil
	}
	field, ok := id.Obj.Decl.(*ast.Field)
	if !ok {
		return nil
	}
	if p, ok := e.params[field]; ok {
		return &p
	}
	return nil
}

func binding(p *param) bool {
	return p != nil && (p.kind == "T" || p.kind == "B")
}

func (e *extractor) classify(id *ast.Ident, stack []ast.Node) (string, *param) {
	p := e.paramOf(id)
	switch {
	case binding(p):
		return "", p
	case p != nil && p.kind == "dot":
		return "dot_import", nil
	case len(e.names) == 0:
		return "no_testing_import", nil
	case e.shadows(id, stack):
		return "shadowed", nil
	case p == nil:
		return "unresolved_receiver", nil
	case p.kind == "TB":
		return "testing_TB", nil
	}
	return "non_testing_type", nil
}

// shadows reports whether a function on the enclosing chain declares a testing parameter named id. In that case id does
// not resolve to that parameter.
func (e *extractor) shadows(id *ast.Ident, stack []ast.Node) bool {
	for _, n := range stack {
		var ft *ast.FuncType
		switch x := n.(type) {
		case *ast.FuncDecl:
			ft = x.Type
		case *ast.FuncLit:
			ft = x.Type
		default:
			continue
		}
		if ft.Params == nil {
			continue
		}
		for _, field := range ft.Params.List {
			if kind := e.params[field].kind; kind != "T" && kind != "B" {
				continue
			}
			for _, name := range field.Names {
				if name.Name == id.Name {
					return true
				}
			}
		}
	}
	return false
}

func (e *extractor) walk(f *ast.File) {
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		switch x := n.(type) {
		case *ast.CallExpr:
			e.call(x, stack)
		case *ast.SelectorExpr:
			e.selector(x, stack)
		}
		stack = append(stack, n)
		return true
	})
}

func (e *extractor) call(c *ast.CallExpr, stack []ast.Node) {
	sel, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr)
	if !ok {
		return
	}
	method := sel.Sel.Name
	id, ident := sel.X.(*ast.Ident)
	var reason string
	var p *param
	if ident {
		reason, p = e.classify(id, stack)
	}
	if !targets[method] && p == nil {
		return
	}
	if !ident {
		reason = "unsupported_receiver_form"
	}
	stmt, target := "other", ast.Node(c)
	switch parent := stack[len(stack)-1].(type) {
	case *ast.ExprStmt:
		stmt, target = "expression", parent
	case *ast.DeferStmt:
		stmt = "defer"
	case *ast.GoStmt:
		stmt = "go"
	}
	decl := enclosingDecl(stack)
	if reason == "" {
		switch {
		case decl == nil:
			reason = "no_enclosing_declaration"
		case !targets[method]:
			reason = "ignored_method"
		case stmt == "defer" || stmt == "go":
			reason = "deferred_or_go"
		case len(c.Args) == 0:
			reason = "empty_arguments"
		case stmt != "expression":
			reason = "not_expression_statement"
		}
	}
	row := CallRow{
		Schema: Schema, Kind: "call", Path: e.path, FileSHA256: e.sha, Unit: "call", Method: method,
		Statement: stmt, Target: e.slice(target.Pos(), target.End()), Sel: offset(e.fset, sel.Sel.Pos()),
	}
	if ident {
		row.Receiver = ptr(id.Name)
	}
	e.file.Calls++
	if reason != "" {
		row.Reason = ptr(reason)
		e.file.Exclusions[reason]++
		e.rows = append(e.rows, row)
		return
	}
	e.file.Admitted++
	e.admit(&row, id, p, decl, stack)
	e.rows = append(e.rows, row)
	e.occs = append(e.occs, Occurrence{
		Row: len(e.rows) - 1, Stmt: target.(*ast.ExprStmt), Call: c, Decl: decl, Owner: p.owner,
	})
}

// selector records a method value: an admitted method on a testing binding that is not the function of a call, inside
// any parentheses.
func (e *extractor) selector(sel *ast.SelectorExpr, stack []ast.Node) {
	if !targets[sel.Sel.Name] {
		return
	}
	i := len(stack) - 1
	for _, paren := stack[i].(*ast.ParenExpr); paren; _, paren = stack[i].(*ast.ParenExpr) {
		i--
	}
	if c, ok := stack[i].(*ast.CallExpr); ok && ast.Unparen(c.Fun) == ast.Expr(sel) {
		return
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || !binding(e.paramOf(id)) {
		return
	}
	e.file.Selectors++
	e.file.Exclusions["method_value"]++
	e.rows = append(e.rows, CallRow{
		Schema: Schema, Kind: "call", Path: e.path, FileSHA256: e.sha, Unit: "selector",
		Method: sel.Sel.Name, Receiver: ptr(id.Name), Statement: "other", Reason: ptr("method_value"),
		Target: e.slice(sel.Pos(), sel.End()), Sel: offset(e.fset, sel.Sel.Pos()),
	})
}

func enclosingDecl(stack []ast.Node) *ast.FuncDecl {
	for _, n := range stack {
		if fd, ok := n.(*ast.FuncDecl); ok {
			return fd
		}
	}
	return nil
}

func (e *extractor) admit(row *CallRow, id *ast.Ident, p *param, fd *ast.FuncDecl, stack []ast.Node) {
	row.Admitted = true
	declStart := fd.Pos()
	if fd.Doc != nil {
		declStart = fd.Doc.Pos()
	}
	decl := &Declaration{Kind: "func", Name: fd.Name.Name, Span: e.slice(declStart, fd.End())}
	if fd.Recv != nil {
		decl.Kind = "method"
	}
	row.Declaration = decl
	owner := Owner{Kind: "declaration", Span: e.slice(p.owner.Pos(), p.owner.End())}
	if _, ok := p.owner.(*ast.FuncLit); ok {
		owner.Kind = "literal"
	}
	row.Binding = &Binding{Name: id.Name, Kind: p.kind, Field: e.slice(p.field.Pos(), p.field.End()), Owner: owner}

	row.Literals = []Literal{}
	callback := false
	var run *ast.CallExpr
	for i, n := range stack {
		lit, ok := n.(*ast.FuncLit)
		if !ok {
			continue
		}
		position, call := e.position(lit, stack[i-1])
		if position == "run_argument" {
			run = call
		} else {
			callback = true
		}
		row.Literals = append(row.Literals, Literal{Position: position, Span: e.slice(lit.Pos(), lit.End())})
	}
	row.Callback = &callback
	reasons := []string{}
	switch {
	case fd.Recv != nil:
		reasons = append(reasons, "method")
	case !testName(fd.Name.Name, "Test") && !testName(fd.Name.Name, "Benchmark"):
		reasons = append(reasons, "helper")
	}
	if callback {
		reasons = append(reasons, "callback")
	}
	sort.Strings(reasons)
	row.ContextReasons = reasons
	context := "complete"
	if len(reasons) > 0 {
		context = "outside_packet"
	}
	row.CallerContext = &context
	switch {
	case run != nil:
		name := run.Args[0]
		span := e.slice(name.Pos(), name.End())
		row.EnclosingRunName = &RunName{Kind: "dynamic", Span: &span}
		if lit, ok := name.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if value, err := strconv.Unquote(lit.Value); err == nil {
				row.EnclosingRunName.Kind, row.EnclosingRunName.Value = "literal", &value
			}
		}
	case context == "complete":
		row.EnclosingRunName = &RunName{Kind: "none"}
	default:
		row.EnclosingRunName = &RunName{Kind: "unavailable"}
	}
}

// position classifies a function literal by its parent node. For a run_argument literal it also returns the Run call.
func (e *extractor) position(lit *ast.FuncLit, parent ast.Node) (string, *ast.CallExpr) {
	switch x := parent.(type) {
	case *ast.CallExpr:
		if len(x.Args) >= 2 && x.Args[1] == lit {
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" {
				if id, ok := sel.X.(*ast.Ident); ok && binding(e.paramOf(id)) {
					return "run_argument", x
				}
			}
		}
	case *ast.ReturnStmt:
		return "returned", nil
	}
	return "other", nil
}

func testName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(name[len(prefix):])
	return len(name) == len(prefix) || !unicode.IsLower(r)
}

func (e *extractor) slice(from, to token.Pos) Slice {
	start, end := offset(e.fset, from), offset(e.fset, to)
	return Slice{
		Start: start, End: end, Line: bytes.Count(e.src[:start], []byte("\n")) + 1,
		SHA256: digest(e.src[start:end]),
	}
}

func offset(fset *token.FileSet, pos token.Pos) int {
	return fset.PositionFor(pos, false).Offset
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ptr(s string) *string {
	return &s
}
