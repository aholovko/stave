package analyzer

import (
	"go/ast"
	"go/token"
	"strings"
)

// checkDeclaredNames reports underscores in declared names other than the blank identifier. Uppercase names without
// underscores are allowed because initialisms such as ID and URL are idiomatic.
func (s *scan) checkDeclaredNames() {
	seen := map[int]bool{}
	for _, id := range s.declaredIdents() {
		if id.Name == "_" || !strings.ContainsRune(id.Name, '_') {
			continue
		}
		off := s.at(id.Pos())
		if seen[off] {
			continue
		}
		seen[off] = true
		s.emitNode("P3", id)
	}
}

func (s *scan) declaredIdents() []*ast.Ident {
	return DeclaringIdents(s.file)
}

// DeclaringIdents returns declaring identifiers in n for exact analysis and structural candidate collection.
// Generic receiver type parameters are excluded here; structural collection adds them separately.
func DeclaringIdents(n ast.Node) []*ast.Ident {
	var out []*ast.Ident
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Field:
			// Parameters, results, receivers, struct fields, interface methods, and type parameters all arrive here.
			out = append(out, x.Names...)
		case *ast.FuncDecl:
			out = append(out, x.Name)
		case *ast.TypeSpec:
			out = append(out, x.Name)
		case *ast.ValueSpec:
			out = append(out, x.Names...)
		case *ast.ImportSpec:
			if x.Name != nil {
				out = append(out, x.Name)
			}
		case *ast.LabeledStmt:
			out = append(out, x.Label)
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				for _, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						out = append(out, id)
					}
				}
			}
		case *ast.RangeStmt:
			if x.Tok == token.DEFINE {
				for _, e := range []ast.Expr{x.Key, x.Value} {
					if id, ok := e.(*ast.Ident); ok {
						out = append(out, id)
					}
				}
			}
		}
		return true
	})
	return out
}
