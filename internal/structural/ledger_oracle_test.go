package structural_test

// The P2/P3 oracle uses only the standard library and its own walks so agreement
// can detect mistakes in the analyzer, snapshot reader and builder.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type oRec struct {
	status   string
	reasons  []string
	anchor   [2]int
	cand     *[2]int
	env      *[2]int
	base     string
	name     string
	subtype  string
	imp      *[2]int
	fileConf [][2]int
	pkgConf  []string
	pre      int
}

// oPkg is the oracle's own package observation: which verified files declare a name at package block, and whether every
// evidence file was verified.
type oPkg struct {
	complete bool
	declares map[string][]string
}

type oracle struct {
	fset *token.FileSet
	file *ast.File
	pre  int
}

func (o *oracle) span(n ast.Node) [2]int { return [2]int{o.off(n.Pos()), o.off(n.End())} }

func (o *oracle) off(p token.Pos) int { return o.fset.File(p).Offset(p) }

func oracleParse(src []byte) (*oracle, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "oracle.go", src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	return &oracle{fset: fset, file: file}, nil
}

// Use an independent directory read and declaration walk to check package evidence.
func oracleReadPkg(dir string) oPkg {
	pkg, _ := oracleReadPkgCounted(dir)
	return pkg
}

// oracleReadPkgCounted is oracleReadPkg, but it also returns the number of bytes that its file reads returned. This
// includes the bytes of a read that ended in an error.
func oracleReadPkgCounted(dir string) (oPkg, int64) {
	var read int64
	pkg := oPkg{complete: true, declares: map[string][]string{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return oPkg{}, read
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			pkg.complete = false
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > 8<<20 {
			pkg.complete = false
			continue
		}
		src, err := os.ReadFile(path)
		read += int64(len(src))
		if err != nil {
			pkg.complete = false
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ParseComments)
		if err != nil {
			pkg.complete = false
			continue
		}
		seen := map[string]bool{}
		add := func(id *ast.Ident) {
			if id.Name != "_" && !seen[id.Name] {
				seen[id.Name] = true
				pkg.declares[id.Name] = append(pkg.declares[id.Name], name)
			}
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.Name != "init" {
					add(d.Name)
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						add(s.Name)
					case *ast.ValueSpec:
						for _, n := range s.Names {
							add(n)
						}
					}
				}
			}
		}
	}
	return pkg, read
}

// oDecl is one identifier at a declaring position.
type oDecl struct {
	name, subtype string
	span, env     [2]int
	importName    bool
	pre           int
}

// declaring independently collects declared names and separates non-identifier receiver arguments.
func (o *oracle) declaring() (decls []oDecl, args []oRec) {
	add := func(id *ast.Ident, subtype string, env [2]int, importName bool) {
		o.pre++
		decls = append(decls, oDecl{name: id.Name, subtype: subtype, span: o.span(id), env: env,
			importName: importName, pre: o.pre})
	}
	fields := func(fl *ast.FieldList, subtype string) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			for _, id := range f.Names {
				add(id, subtype, o.span(f), false)
			}
		}
	}
	var recvs []*ast.FuncDecl
	ast.Inspect(o.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			sub := "func"
			if n.Recv != nil {
				sub = "method"
				recvs = append(recvs, n)
			}
			add(n.Name, sub, [2]int{o.off(n.Type.Func), o.off(n.Type.End())}, false)
			fields(n.Recv, "receiver")
		case *ast.FuncType:
			fields(n.TypeParams, "type_param")
			fields(n.Params, "param")
			fields(n.Results, "result")
		case *ast.StructType:
			fields(n.Fields, "struct_field")
		case *ast.InterfaceType:
			fields(n.Methods, "interface_method")
		case *ast.TypeSpec:
			add(n.Name, "type", o.span(n), false)
			fields(n.TypeParams, "type_param")
		case *ast.GenDecl:
			for _, s := range n.Specs {
				switch s := s.(type) {
				case *ast.ImportSpec:
					if s.Name != nil {
						add(s.Name, "import", o.span(s), true)
					}
				case *ast.ValueSpec:
					end := s.Names[len(s.Names)-1].End()
					if s.Type != nil {
						end = s.Type.End()
					}
					for _, id := range s.Names {
						add(id, strings.ToLower(n.Tok.String()), [2]int{o.off(s.Names[0].Pos()), o.off(end)}, false)
					}
				}
			}
		case *ast.LabeledStmt:
			add(n.Label, "label", [2]int{o.off(n.Label.Pos()), o.off(n.Colon) + 1}, false)
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				for _, e := range n.Lhs {
					if id, ok := e.(*ast.Ident); ok {
						add(id, "define", [2]int{o.off(n.Lhs[0].Pos()), o.off(n.TokPos) + 2}, false)
					}
				}
			}
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				for _, e := range []ast.Expr{n.Key, n.Value} {
					if id, ok := e.(*ast.Ident); ok {
						add(id, "range", [2]int{o.off(n.Key.Pos()), o.off(n.TokPos) + 2}, false)
					}
				}
			}
		}
		return true
	})
	starts := map[int]bool{}
	for _, d := range decls {
		starts[d.span[0]] = true
	}
	for _, fd := range recvs {
		for _, f := range fd.Recv.List {
			t := stripParens(f.Type)
			if star, ok := t.(*ast.StarExpr); ok {
				t = stripParens(star.X)
			}
			var list []ast.Expr
			switch t := t.(type) {
			case *ast.IndexExpr:
				list = []ast.Expr{t.Index}
			case *ast.IndexListExpr:
				list = t.Indices
			}
			for _, a := range list {
				if id, ok := a.(*ast.Ident); ok {
					if !starts[o.off(id.Pos())] {
						starts[o.off(id.Pos())] = true
						add(id, "receiver_type_parameter", o.span(f), false)
					}
					continue
				}
				o.pre++
				args = append(args, oRec{status: "excluded_receiver_argument",
					reasons: []string{"excluded_receiver_argument"}, anchor: o.span(a), pre: o.pre})
			}
		}
	}
	return decls, args
}

func stripParens(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

func oracleP3(src []byte) ([]oRec, error) {
	o, err := oracleParse(src)
	if err != nil {
		return nil, err
	}
	decls, recs := o.declaring()
	for _, d := range decls {
		r := oRec{anchor: d.span, name: d.name, subtype: d.subtype, pre: d.pre}
		switch d.name {
		case "_":
			r.status = "excluded_blank"
		case ".":
			r.status = "excluded_dot_import"
		default:
			r.status = "candidate"
			cand, env := d.span, d.env
			r.cand, r.env = &cand, &env
		}
		r.reasons = []string{r.status}
		recs = append(recs, r)
	}
	return oSort(recs), nil
}

var oCallees = map[string]string{"errors": "New", "fmt": "Errorf"}

// oracleP2 enumerates and classifies P2 calls. pkg is nil without package evidence.
func oracleP2(src []byte, pkg *oPkg) ([]oRec, error) {
	o, err := oracleParse(src)
	if err != nil {
		return nil, err
	}
	first := map[string]*ast.ImportSpec{}
	for _, d := range o.file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		for _, s := range gd.Specs {
			spec := s.(*ast.ImportSpec)
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				path = spec.Path.Value
			}
			if first[path] == nil {
				first[path] = spec
			}
		}
	}
	decls, _ := o.declaring()
	var recs []oRec
	ast.Inspect(o.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := stripParens(call.Fun).(*ast.SelectorExpr)
		if !ok {
			return true
		}
		parenBase := false
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			if _, paren := sel.X.(*ast.ParenExpr); paren {
				id, ok = stripParens(sel.X).(*ast.Ident)
				parenBase = ok
			}
		}
		if !ok || oCallees[id.Name] != sel.Sel.Name {
			return true
		}
		o.pre++
		r := oRec{base: id.Name, anchor: o.span(call), pre: o.pre}
		var lit *ast.BasicLit
		if len(call.Args) > 0 {
			r.anchor = o.span(call.Args[0])
			if l, ok := call.Args[0].(*ast.BasicLit); ok && l.Kind == token.STRING {
				lit = l
			}
		}
		if parenBase {
			r.reasons = append(r.reasons, "excluded_paren_base")
		}
		if len(call.Args) == 0 {
			r.reasons = append(r.reasons, "no_argument")
		} else if lit == nil {
			r.reasons = append(r.reasons, "indirect_argument")
		} else if _, err := strconv.Unquote(lit.Value); err != nil {
			r.reasons = append(r.reasons, "undecodable")
		}
		spec := first[id.Name]
		if spec != nil {
			imp := o.span(spec)
			r.imp = &imp
		}
		if spec == nil || spec.Name != nil {
			r.reasons = append(r.reasons, "excluded_import")
		}
		for _, d := range decls {
			if !d.importName && d.name == id.Name {
				r.fileConf = append(r.fileConf, d.span)
			}
		}
		sort.Slice(r.fileConf, func(i, j int) bool { return r.fileConf[i][0] < r.fileConf[j][0] })
		if len(r.fileConf) > 0 {
			r.reasons = append(r.reasons, "excluded_file_conflict")
		}
		if pkg != nil {
			r.pkgConf = pkg.declares[id.Name]
		}
		if len(r.pkgConf) > 0 {
			r.reasons = append(r.reasons, "excluded_package_conflict")
		}
		if (pkg == nil || !pkg.complete) && len(r.fileConf) == 0 && len(r.pkgConf) == 0 {
			r.reasons = append(r.reasons, "unknown_context")
		}
		if len(r.reasons) == 0 {
			r.reasons = []string{"eligible"}
		}
		r.status = r.reasons[0]
		if !parenBase && lit != nil {
			cand, env := o.span(lit), [2]int{o.off(call.Pos()), o.off(lit.End())}
			r.cand, r.env = &cand, &env
		}
		recs = append(recs, r)
		return true
	})
	return oSort(recs), nil
}

func oSort(recs []oRec) []oRec {
	sort.Slice(recs, func(i, j int) bool {
		a, b := recs[i], recs[j]
		if a.anchor != b.anchor {
			if a.anchor[0] != b.anchor[0] {
				return a.anchor[0] < b.anchor[0]
			}
			return a.anchor[1] < b.anchor[1]
		}
		return a.pre < b.pre
	})
	return recs
}
