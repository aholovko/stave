package diag

import "bytes"

// LineCol presents a byte offset the way Go tools print positions: a 1-based line counted by LF and a 1-based column
// counted in bytes. It is a presentation of a span and never part of the identity of a finding.
func LineCol(src []byte, off int) (line, col int) {
	off = max(0, min(off, len(src)))
	line = 1 + bytes.Count(src[:off], []byte{'\n'})
	col = off - bytes.LastIndexByte(src[:off], '\n')
	return line, col
}
