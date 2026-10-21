package tfcand

// Context derivation uses only the supplied declaration and testing import names.
// Whole-file eligibility is the caller's responsibility; no cross-file lookup or
// alias inference is attempted.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
)

// The synthetic package clause lets the parser accept a declaration on its own.
// Reported offsets exclude this prefix.
const contextPrefix = "package p\n"

// Limit branching when a testing parameter reaches several local call sites.
const maxPaths = 16

// ContextRequest supplies a declaration, target byte range and testing imports for context derivation.
type ContextRequest struct {
	Declaration    string   `json:"declaration"`
	Target         []int    `json:"target"`
	ID             string   `json:"id"`
	TestingImports []string `json:"testing_imports"`
	DotImport      bool     `json:"dot_import"`
}

// ContextResult is either derived, with a context, or attribute_underivable, with a reason. An underivable candidate is
// retained and counted, but never selected.
type ContextResult struct {
	Status  string   `json:"status"`
	Reason  string   `json:"reason,omitempty"`
	Context *Context `json:"context,omitempty"`
}

// Context is the prompt context object in contract key order. Byte ranges are zero-based, half-open and
// declaration-relative.
type Context struct {
	ID                     string           `json:"id"`
	CallerContext          string           `json:"caller_context"`
	ContextReasons         []string         `json:"context_reasons"`
	Callback               bool             `json:"callback"`
	Literals               []ContextLiteral `json:"literals"`
	ReachedLiterals        []ContextLiteral `json:"reached_literals"`
	EnclosingRunName       ContextRunName   `json:"enclosing_run_name"`
	ReportedName           ReportedName     `json:"reported_name"`
	TargetDeclarationBytes []int            `json:"target_declaration_bytes"`
}

// ContextLiteral is a chain literal (OwnsBinding set) or a reached off-chain owner (OwnsBinding nil). CallSites is set
// only for local_visible.
type ContextLiteral struct {
	Position         string  `json:"position"`
	Invocation       string  `json:"invocation"`
	OwnsBinding      *bool   `json:"owns_binding,omitempty"`
	DeclarationBytes []int   `json:"declaration_bytes"`
	CallSites        [][]int `json:"call_sites,omitempty"`
}

// ContextRunName records the innermost syntactic Run name, without evaluating it.
type ContextRunName struct {
	Kind       string  `json:"kind"`
	Value      *string `json:"value,omitempty"`
	Expression *string `json:"expression,omitempty"`
}

// ReportedName holds every path from the binding to where it ends.
type ReportedName struct {
	Paths []ReportedPath `json:"paths"`
}

// ReportedPath ends in_packet or caller_supplied at EndBytes, with the Run names collected on the way, outermost first.
type ReportedPath struct {
	End      string        `json:"end"`
	EndBytes []int         `json:"end_bytes"`
	RunNames []PathRunName `json:"run_names"`
}

// PathRunName is a literal or dynamic Run name argument on a path.
type PathRunName struct {
	Kind             string  `json:"kind"`
	Value            *string `json:"value,omitempty"`
	Expression       *string `json:"expression,omitempty"`
	DeclarationBytes []int   `json:"declaration_bytes"`
}

// literalInfo is one classified literal and its direct call sites.
type literalInfo struct {
	out   ContextLiteral
	sites []*ast.CallExpr
}

type deriver struct {
	e       *extractor
	base    int
	parents map[ast.Node]ast.Node
	decl    *ast.FuncDecl
	chain   map[*ast.FuncLit]bool
	info    map[*ast.FuncLit]*literalInfo
	reached []ContextLiteral
	seen    map[*ast.FuncLit]bool
	lits    int
	paths   []ReportedPath
}

// DeriveContext derives the target's declaration context under the same admission
// rules as Screen. Unresolved context remains attribute_underivable, not a negative.
func DeriveContext(req ContextRequest) ContextResult {
	c, reason := deriveContext(req)
	if reason != "" {
		return ContextResult{Status: "attribute_underivable", Reason: reason}
	}
	return ContextResult{Status: "derived", Context: c}
}

func deriveContext(req ContextRequest) (*Context, string) {
	if req.DotImport {
		return nil, "dot_import"
	}
	t := req.Target
	if len(t) != 2 || t[0] < 0 || t[0] > t[1] || t[1] > len(req.Declaration) {
		return nil, "span_mismatch"
	}
	src := []byte(contextPrefix + req.Declaration)
	base := len(contextPrefix)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, ParserMode)
	if err != nil {
		return nil, "parse_error"
	}
	if len(f.Decls) != 1 {
		return nil, "span_mismatch"
	}
	fd, ok := f.Decls[0].(*ast.FuncDecl)
	if !ok {
		return nil, "span_mismatch"
	}
	start := fd.Pos()
	if fd.Doc != nil {
		start = fd.Doc.Pos()
	}
	if offset(fset, start) != base || offset(fset, fd.End()) != len(src) {
		return nil, "span_mismatch"
	}

	e := &extractor{
		src: src, fset: fset, names: map[string]bool{}, params: map[*ast.Field]param{},
		file: &FileRow{Exclusions: map[string]int{}},
	}
	for _, name := range req.TestingImports {
		e.names[name] = true
	}
	e.declare(f)
	row, call, stack := e.target(f, base+t[0], base+t[1])
	if row == nil || !row.Admitted {
		return nil, "target_not_admitted"
	}

	d := &deriver{
		e: e, base: base, parents: map[ast.Node]ast.Node{}, decl: fd, chain: map[*ast.FuncLit]bool{},
		info: map[*ast.FuncLit]*literalInfo{}, reached: []ContextLiteral{}, seen: map[*ast.FuncLit]bool{},
	}
	var up []ast.Node
	ast.Inspect(fd, func(n ast.Node) bool {
		if n == nil {
			up = up[:len(up)-1]
			return true
		}
		if len(up) > 0 {
			d.parents[n] = up[len(up)-1]
		}
		if _, ok := n.(*ast.FuncLit); ok {
			d.lits++
		}
		up = append(up, n)
		return true
	})

	id := ast.Unparen(call.Fun).(*ast.SelectorExpr).X.(*ast.Ident)
	p := e.paramOf(id)
	literals := []ContextLiteral{}
	for _, n := range stack {
		if lit, ok := n.(*ast.FuncLit); ok {
			d.chain[lit] = true
		}
	}
	for _, n := range stack {
		lit, ok := n.(*ast.FuncLit)
		if !ok {
			continue
		}
		info, reason := d.classify(lit)
		if reason != "" {
			return nil, reason
		}
		out := info.out
		owns := p.owner == ast.Node(lit)
		out.OwnsBinding = &owns
		literals = append(literals, out)
	}
	if reason := d.follow(p, id.Name, nil, 0); reason != "" {
		return nil, reason
	}

	name := ContextRunName{Kind: row.EnclosingRunName.Kind, Value: row.EnclosingRunName.Value}
	if name.Kind == "dynamic" {
		s := row.EnclosingRunName.Span
		name.Expression = ptr(string(src[s.Start:s.End]))
	}
	return &Context{
		ID: req.ID, CallerContext: *row.CallerContext, ContextReasons: row.ContextReasons, Callback: *row.Callback,
		Literals: literals, ReachedLiterals: d.reached, EnclosingRunName: name,
		ReportedName: ReportedName{Paths: d.paths}, TargetDeclarationBytes: []int{t[0], t[1]},
	}, ""
}

// target reuses the admission screen so context derivation cannot broaden eligibility.
func (e *extractor) target(f *ast.File, start, end int) (*CallRow, *ast.CallExpr, []ast.Node) {
	var stack []ast.Node
	var row *CallRow
	var found *ast.CallExpr
	var at []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if c, ok := n.(*ast.CallExpr); ok && found == nil {
			if s, ok := stack[len(stack)-1].(*ast.ExprStmt); ok &&
				offset(e.fset, s.Pos()) == start && offset(e.fset, s.End()) == end {
				found, at = c, append([]ast.Node(nil), stack...)
				before := len(e.rows)
				if e.call(c, at); len(e.rows) > before {
					row = &e.rows[len(e.rows)-1]
				}
			}
		}
		stack = append(stack, n)
		return true
	})
	return row, found, at
}

// classify records a literal's position and invocation. run_argument follows its position.
// A sole literal value in a one-name := or var is local_visible when every other use is a direct call.
// An otherwise eligible binding with no calls returns zero_call_sites; other forms are escaping.
func (d *deriver) classify(lit *ast.FuncLit) (*literalInfo, string) {
	if info, ok := d.info[lit]; ok {
		return info, ""
	}
	position, _ := d.e.position(lit, d.parents[lit])
	info := &literalInfo{out: ContextLiteral{Position: position, Invocation: "escaping", DeclarationBytes: d.rel(lit.Pos(), lit.End())}}
	if position == "run_argument" {
		info.out.Invocation = "run_argument"
	} else if v := localName(lit, d.parents[lit]); v != nil {
		if sites, ok := d.calls(v); ok {
			if len(sites) == 0 {
				return nil, "zero_call_sites"
			}
			info.out.Invocation, info.sites = "local_visible", sites
			for _, s := range sites {
				info.out.CallSites = append(info.out.CallSites, d.rel(s.Pos(), s.End()))
			}
		}
	}
	d.info[lit] = info
	return info, ""
}

// localName returns v for `v := lit` or `var v [T] = lit` with one name and one value, else nil.
func localName(lit *ast.FuncLit, parent ast.Node) *ast.Ident {
	var v *ast.Ident
	switch x := parent.(type) {
	case *ast.AssignStmt:
		if x.Tok == token.DEFINE && len(x.Lhs) == 1 && len(x.Rhs) == 1 && x.Rhs[0] == lit {
			v, _ = x.Lhs[0].(*ast.Ident)
		}
	case *ast.ValueSpec:
		if len(x.Names) == 1 && len(x.Values) == 1 && x.Values[0] == lit {
			v = x.Names[0]
		}
	}
	if v == nil || v.Name == "_" || v.Obj == nil {
		return nil
	}
	return v
}

// calls returns the direct calls of v in source order. It returns false when any other use (assignment, increment,
// address, argument, ...) exists.
func (d *deriver) calls(v *ast.Ident) ([]*ast.CallExpr, bool) {
	var sites []*ast.CallExpr
	ok := true
	ast.Inspect(d.decl, func(n ast.Node) bool {
		id, isID := n.(*ast.Ident)
		if !isID || id == v || id.Obj != v.Obj {
			return true
		}
		if c, isCall := d.parents[id].(*ast.CallExpr); isCall && c.Fun == ast.Expr(id) {
			sites = append(sites, c)
		} else {
			ok = false
		}
		return true
	})
	return sites, ok
}

// follow continues a path at parameter name of p. Names are collected innermost first. steps counts the literal owners.
func (d *deriver) follow(p *param, name string, names []PathRunName, steps int) string {
	if p == nil {
		return "missing_owner"
	}
	ident := paramName(p.field, name)
	if ident == nil {
		return "missing_owner"
	}
	switch owner := p.owner.(type) {
	case *ast.FuncDecl:
		end := "caller_supplied"
		if owner.Recv == nil && (testName(owner.Name.Name, "Test") || testName(owner.Name.Name, "Benchmark")) {
			end = "in_packet"
		}
		return d.emit(end, ident, names)
	case *ast.FuncLit:
		if steps++; steps > d.lits {
			return "owner_depth_cap"
		}
		info, reason := d.classify(owner)
		if reason != "" {
			return reason
		}
		if !d.chain[owner] && !d.seen[owner] {
			d.seen[owner] = true
			d.reached = append(d.reached, info.out)
		}
		switch info.out.Invocation {
		case "run_argument":
			run := d.parents[owner].(*ast.CallExpr)
			names = append(append([]PathRunName(nil), names...), d.runName(run.Args[0]))
			rid := run.Fun.(*ast.SelectorExpr).X.(*ast.Ident)
			return d.follow(d.e.paramOf(rid), rid.Name, names, steps)
		case "local_visible":
			index := paramIndex(owner, ident)
			if index < 0 {
				return "missing_owner"
			}
			for _, site := range info.sites {
				if site.Ellipsis.IsValid() && index >= len(site.Args)-1 || index >= len(site.Args) {
					return "call_site_argument_missing"
				}
				arg := site.Args[index]
				if aid, ok := arg.(*ast.Ident); ok {
					if ap := d.e.paramOf(aid); binding(ap) {
						if reason := d.follow(ap, aid.Name, names, steps); reason != "" {
							return reason
						}
						continue
					}
				}
				if reason := d.emit("caller_supplied", arg, names); reason != "" {
					return reason
				}
			}
			return ""
		default:
			return d.emit("caller_supplied", ident, names)
		}
	}
	return "missing_owner"
}

func (d *deriver) emit(end string, at ast.Node, names []PathRunName) string {
	if len(d.paths) == maxPaths {
		return "path_cap"
	}
	outer := make([]PathRunName, 0, len(names))
	for i := len(names) - 1; i >= 0; i-- {
		outer = append(outer, names[i])
	}
	d.paths = append(d.paths, ReportedPath{End: end, EndBytes: d.rel(at.Pos(), at.End()), RunNames: outer})
	return ""
}

func (d *deriver) runName(arg ast.Expr) PathRunName {
	n := PathRunName{Kind: "dynamic", DeclarationBytes: d.rel(arg.Pos(), arg.End())}
	if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		if value, err := strconv.Unquote(lit.Value); err == nil {
			n.Kind, n.Value = "literal", &value
			return n
		}
	}
	n.Expression = ptr(string(d.e.src[offset(d.e.fset, arg.Pos()):offset(d.e.fset, arg.End())]))
	return n
}

func (d *deriver) rel(from, to token.Pos) []int {
	return []int{offset(d.e.fset, from) - d.base, offset(d.e.fset, to) - d.base}
}

func paramName(field *ast.Field, name string) *ast.Ident {
	for _, n := range field.Names {
		if n.Name == name {
			return n
		}
	}
	return nil
}

func paramIndex(lit *ast.FuncLit, name *ast.Ident) int {
	i := 0
	for _, field := range lit.Type.Params.List {
		if len(field.Names) == 0 {
			i++
		}
		for _, n := range field.Names {
			if n == name {
				return i
			}
			i++
		}
	}
	return -1
}
