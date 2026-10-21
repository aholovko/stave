package analyzer

import (
	"go/ast"

	"github.com/aholovko/stave/internal/lex"
)

// checkElseAfterReturn reports an else after an if-body ending in a direct return. Other terminating statements are
// outside this predicate. The anchor comes from lexical records because the AST has no else-keyword position and
// a text search could match a comment.
func (s *scan) checkElseAfterReturn() {
	toks := s.inst.Tokens()
	ast.Inspect(s.file, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok || stmt.Else == nil || stmt.Body == nil || len(stmt.Body.List) == 0 {
			return true
		}
		if _, ok := stmt.Body.List[len(stmt.Body.List)-1].(*ast.ReturnStmt); !ok {
			return true
		}
		from, to := s.at(stmt.Body.End()), s.at(stmt.Else.Pos())
		if from < 0 || to < from {
			return true
		}
		if off, ok := findKeyword(toks, "else", from, to); ok {
			s.emit("P1", off, off+len("else"))
		}
		return true
	})
}

// findKeyword returns the offset of the first keyword record with the given text whose position lies in [from, to).
func findKeyword(toks []lex.Token, text string, from, to int) (int, bool) {
	for _, t := range toks {
		if t.Off >= to {
			break
		}
		if t.Off >= from && t.Kind == lex.Keyword && t.Text == text {
			return t.Off, true
		}
	}
	return 0, false
}
