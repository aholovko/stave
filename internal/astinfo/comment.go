package astinfo

import (
	"bytes"
	"go/ast"
	"go/token"

	"github.com/aholovko/stave/internal/span"
)

// CommentGroupSpan reads source bytes because go/ast removes carriage returns from comment text. Line comments
// exclude the trailing CR and LF; block comments include the closing "*/".
func CommentGroupSpan(src []byte, fset *token.FileSet, group *ast.CommentGroup) span.Span {
	first := group.List[0]
	last := group.List[len(group.List)-1]
	return span.Span{Start: offset(fset, first.Slash), End: commentEnd(src, offset(fset, last.Slash))}
}

func commentEnd(src []byte, start int) int {
	rest := src[start:]
	if bytes.HasPrefix(rest, []byte("/*")) {
		if i := bytes.Index(rest[2:], []byte("*/")); i >= 0 {
			return start + 2 + i + 2
		}
		return len(src) // unterminated: the parser already reported an error
	}
	end := len(src)
	if i := bytes.IndexByte(rest, '\n'); i >= 0 {
		end = start + i
	}
	if end > start && src[end-1] == '\r' {
		end--
	}
	return end
}

func offset(fset *token.FileSet, pos token.Pos) int {
	return fset.PositionFor(pos, false).Offset
}
