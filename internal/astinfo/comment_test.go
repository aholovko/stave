package astinfo

import (
	"go/parser"
	"go/token"
	"testing"
)

// CRLF comments require source offsets rather than normalized comment lengths.
func TestCommentGroupSpanIsTheOriginalBytes(t *testing.T) {
	for name, tc := range map[string]struct{ src, want string }{
		"CRLF line comments keep interior CR and drop the trailing CR": {
			"package p\r\n\r\n// Save writes.\r\n// It really does.\r\nfunc Save() {}\r\n",
			"// Save writes.\r\n// It really does.",
		},
		"a CRLF block comment reaches its closing delimiter": {
			"package p\r\n\r\n/*\r\nEncode writes a frame.\r\nIt is framed.\r\n*/\r\nfunc Encode() {}\r\n",
			"/*\r\nEncode writes a frame.\r\nIt is framed.\r\n*/",
		},
		"a mixed group ends at its last line comment": {
			"package p\r\n\r\n/* Mix starts */\r\n// and ends here\r\nfunc Mix() {}\r\n",
			"/* Mix starts */\r\n// and ends here",
		},
		"multibyte text is counted in bytes": {
			"package p\n\n// Grüß écrit 日本語 and 🎉 loudly.\nfunc Grüß() {}\n",
			"// Grüß écrit 日本語 and 🎉 loudly.",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "", tc.src, ParserMode)
			if err != nil {
				t.Fatal(err)
			}
			s := CommentGroupSpan([]byte(tc.src), fset, file.Comments[0])
			if got := tc.src[s.Start:s.End]; got != tc.want {
				t.Errorf("span [%d, %d) = %q, want %q", s.Start, s.End, got, tc.want)
			}
		})
	}
}
