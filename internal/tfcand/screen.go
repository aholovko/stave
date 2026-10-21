package tfcand

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"unicode/utf8"
)

// Parsed holds a screened source's status, rows and admitted AST nodes in row order.
// Fset is nil for a file that is not checked.
type Parsed struct {
	Fset     *token.FileSet
	Row      FileRow
	Rows     []CallRow
	Admitted []Occurrence
	e        *extractor
}

// Occurrence is one admitted row. Row indexes Parsed.Rows. Stmt is the target expression statement and Call is its
// call. Decl is the enclosing named declaration. Owner is the FuncDecl or FuncLit that declares the testing parameter
// of the receiver.
type Occurrence struct {
	Row   int
	Stmt  *ast.ExprStmt
	Call  *ast.CallExpr
	Decl  *ast.FuncDecl
	Owner ast.Node
}

// RunArg is a function literal that is the direct second argument of a Run call on a testing binding. Name is the first
// argument of the call. Owner is the FuncDecl or FuncLit that declares the parameter of the Run receiver.
type RunArg struct {
	Name  ast.Expr
	Owner ast.Node
}

// Screen screens one source and keeps the admitted nodes for the TF builder. It fails closed: invalid UTF-8, a parse
// error or a generated file yields a status and no occurrences.
func Screen(path string, src []byte) Parsed {
	sha := digest(src)
	file := FileRow{
		Schema: Schema, Kind: "file", Path: path, SHA256: &sha, Bytes: len(src),
		TestingImports: []string{}, Exclusions: map[string]int{},
	}
	if !utf8.Valid(src) {
		file.Status, file.Reason = "invalid_utf8", ptr("source is not valid UTF-8")
		return Parsed{Row: file}
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, ParserMode)
	if err != nil {
		file.Status, file.Reason = "parse_error", ptr(err.Error())
		return Parsed{Row: file}
	}
	file.Package = ptr(f.Name.Name)
	if ast.IsGenerated(f) {
		file.Status, file.Generated, file.Reason = "generated", true, ptr("the standard detector marks the file generated")
		return Parsed{Row: file}
	}
	file.Status = "checked"
	e := &extractor{
		src: src, fset: fset, path: path, sha: sha, names: map[string]bool{},
		params: map[*ast.Field]param{}, file: &file,
	}
	e.imports(f)
	e.declare(f)
	e.walk(f)
	order := make([]int, len(e.rows))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := e.rows[order[i]].Target, e.rows[order[j]].Target
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		return a.End < b.End
	})
	var rows []CallRow // Preserve the nil-slice contract for empty results.
	if len(order) > 0 {
		rows = make([]CallRow, len(order))
	}
	moved := make([]int, len(order))
	for k, i := range order {
		rows[k], moved[i] = e.rows[i], k
	}
	for i := range e.occs {
		e.occs[i].Row = moved[e.occs[i].Row]
	}
	sort.Slice(e.occs, func(i, j int) bool { return e.occs[i].Row < e.occs[j].Row })
	e.rows = rows
	return Parsed{Fset: fset, Row: file, Rows: rows, Admitted: e.occs, e: e}
}

// RunArgs returns syntactic Run-argument literals in fd. It does not infer caller context.
func (p *Parsed) RunArgs(fd *ast.FuncDecl) map[*ast.FuncLit]RunArg {
	out := map[*ast.FuncLit]RunArg{}
	if p.e == nil || fd == nil {
		return out
	}
	var stack []ast.Node
	ast.Inspect(fd, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if lit, ok := n.(*ast.FuncLit); ok {
			if position, call := p.e.position(lit, stack[len(stack)-1]); position == "run_argument" {
				id := call.Fun.(*ast.SelectorExpr).X.(*ast.Ident)
				out[lit] = RunArg{Name: call.Args[0], Owner: p.e.paramOf(id).owner}
			}
		}
		stack = append(stack, n)
		return true
	})
	return out
}
