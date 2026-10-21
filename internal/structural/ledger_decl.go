package structural

import (
	"fmt"
	"go/ast"
	"go/token"

	"github.com/aholovko/stave/internal/analyzer"
)

// decl holds a declaring identifier and its source envelope. A non-identifier
// receiver argument has id nil and appears only in the ledger.
type decl struct {
	id      *ast.Ident
	subtype string
	env     [2]int
	anchor  [2]int
}

// Include receiver type parameters omitted by DeclaringIdents, while preserving
// its records when both walks find the same identifier. Every identifier needs an
// owning declaration to define its envelope.
func (b *builder) declaring(file *ast.File) ([]decl, error) {
	// The supplement runs first. Its nil-argument check must precede any ast.Walk, which panics on a nil node.
	supplement, err := b.receiverSupplement(file)
	if err != nil {
		return nil, err
	}
	owners, err := b.owners(file)
	if err != nil {
		return nil, err
	}
	var out []decl
	seen := map[int]bool{}
	for _, id := range analyzer.DeclaringIdents(file) {
		d, ok := owners[id]
		if !ok {
			return nil, fmt.Errorf("declaring identifier %q at %d has no owner", id.Name, b.off(id.Pos()))
		}
		if !seen[d.anchor[0]] {
			seen[d.anchor[0]] = true
			out = append(out, d)
		}
	}
	for _, d := range supplement {
		if d.id != nil {
			if seen[d.anchor[0]] {
				continue
			}
			seen[d.anchor[0]] = true
		}
		out = append(out, d)
	}
	return out, nil
}

func (b *builder) span(n ast.Node) [2]int { return [2]int{b.off(n.Pos()), b.off(n.End())} }

func (b *builder) owners(file *ast.File) (map[*ast.Ident]decl, error) {
	owners := map[*ast.Ident]decl{}
	var stack []ast.Node
	var err error
	set := func(id *ast.Ident, subtype string, s, t int) {
		owners[id] = decl{id: id, subtype: subtype, env: [2]int{s, t}, anchor: b.span(id)}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if err != nil {
			return false
		}
		switch x := n.(type) {
		case *ast.Field:
			var sub string
			if sub, err = fieldSubtype(stack); err != nil {
				return false
			}
			for _, id := range x.Names {
				set(id, sub, b.off(x.Pos()), b.off(x.End()))
			}
		case *ast.FuncDecl:
			sub := "func"
			if x.Recv != nil {
				sub = "method"
			}
			set(x.Name, sub, b.off(x.Pos()), b.off(x.Type.End()))
		case *ast.TypeSpec:
			set(x.Name, "type", b.off(x.Pos()), b.off(x.End()))
		case *ast.ValueSpec:
			gd, ok := stack[len(stack)-1].(*ast.GenDecl)
			if !ok {
				err = fmt.Errorf("value spec at %d outside a declaration", b.off(x.Pos()))
				return false
			}
			end := x.Names[len(x.Names)-1].End()
			if x.Type != nil {
				end = x.Type.End()
			}
			sub := map[token.Token]string{token.CONST: "const", token.VAR: "var"}[gd.Tok]
			for _, id := range x.Names {
				set(id, sub, b.off(x.Pos()), b.off(end))
			}
		case *ast.ImportSpec:
			if x.Name != nil {
				set(x.Name, "import", b.off(x.Pos()), b.off(x.End()))
			}
		case *ast.LabeledStmt:
			set(x.Label, "label", b.off(x.Label.Pos()), b.off(x.Colon)+1)
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				for _, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						set(id, "define", b.off(x.Lhs[0].Pos()), b.off(x.TokPos)+2)
					}
				}
			}
		case *ast.RangeStmt:
			if x.Tok == token.DEFINE {
				for _, e := range []ast.Expr{x.Key, x.Value} {
					if id, ok := e.(*ast.Ident); ok {
						set(id, "range", b.off(x.Key.Pos()), b.off(x.TokPos)+2)
					}
				}
			}
		}
		stack = append(stack, n)
		return true
	})
	return owners, err
}

func fieldSubtype(stack []ast.Node) (string, error) {
	if len(stack) < 2 {
		return "", fmt.Errorf("field outside a field list")
	}
	fl, ok := stack[len(stack)-1].(*ast.FieldList)
	if !ok {
		return "", fmt.Errorf("field outside a field list")
	}
	switch o := stack[len(stack)-2].(type) {
	case *ast.FuncDecl:
		if o.Recv == fl {
			return "receiver", nil
		}
	case *ast.FuncType:
		switch fl {
		case o.TypeParams:
			return "type_param", nil
		case o.Params:
			return "param", nil
		case o.Results:
			return "result", nil
		}
	case *ast.StructType:
		return "struct_field", nil
	case *ast.InterfaceType:
		return "interface_method", nil
	case *ast.TypeSpec:
		if o.TypeParams == fl {
			return "type_param", nil
		}
	}
	return "", fmt.Errorf("field list of unknown owner %T", stack[len(stack)-2])
}

// Mirror go/types unpackRecv so generic receiver parameters are recognized through
// parentheses and a pointer. Non-identifier arguments stay in the ledger without
// becoming candidates; nil arguments indicate a malformed AST.
func (b *builder) receiverSupplement(file *ast.File) ([]decl, error) {
	var out []decl
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil {
			continue
		}
		for _, field := range fd.Recv.List {
			t := ast.Unparen(field.Type)
			if star, ok := t.(*ast.StarExpr); ok {
				t = ast.Unparen(star.X)
			}
			var args []ast.Expr
			switch x := t.(type) {
			case *ast.IndexExpr:
				args = []ast.Expr{x.Index}
			case *ast.IndexListExpr:
				args = x.Indices
			}
			for _, arg := range args {
				switch a := arg.(type) {
				case nil:
					return nil, fmt.Errorf("receiver of %s has a nil type argument", fd.Name.Name)
				case *ast.Ident:
					out = append(out, decl{id: a, subtype: "receiver_type_parameter", env: b.span(field),
						anchor: b.span(a)})
				default:
					out = append(out, decl{anchor: b.span(a)})
				}
			}
		}
	}
	return out, nil
}
