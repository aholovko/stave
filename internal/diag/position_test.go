package diag

import (
	"strings"
	"testing"
)

// The source mixes a multibyte line, a CRLF line and no final newline, so the byte-column rule is visible: a column
// counts bytes, and a CR belongs to the line it ends.
const positions = "package demo\n// é😀\r\nvar x int = 1"

func TestLineColCountsNewlinesAndByteColumns(t *testing.T) {
	for _, tc := range []struct {
		needle    string
		line, col int
	}{
		{"package", 1, 1},
		{"demo", 1, 9},
		{"//", 2, 1},
		{"😀", 2, 6}, // after "// " and the two bytes of é
		{"var", 3, 1},
		{"int", 3, 7},
	} {
		off := strings.Index(positions, tc.needle)
		if line, col := LineCol([]byte(positions), off); line != tc.line || col != tc.col {
			t.Errorf("LineCol(%q at %d) = %d:%d, want %d:%d", tc.needle, off, line, col, tc.line, tc.col)
		}
	}
}
