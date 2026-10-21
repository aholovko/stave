package lsp

import (
	"sort"
	"unicode/utf8"
)

// position has a zero-based line and column; the column uses the negotiated encoding.
type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// lspRange is an LSP range: half-open, end exclusive.
type lspRange struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

// Preserve the client's preference order; LSP defaults to UTF-16.
func chooseEncoding(offered []string) string {
	for _, enc := range offered {
		switch enc {
		case "utf-8", "utf-16", "utf-32":
			return enc
		}
	}
	return "utf-16"
}

// LSP recognizes LF, CRLF and lone CR, unlike Go's LF-only positions.
// Keep source byte spans unchanged and convert only for presentation.
type lineIndex struct {
	src    []byte
	starts []int // byte offset of the first byte of each line
	enc    string
}

func newLineIndex(src []byte, enc string) *lineIndex {
	starts := []int{0}
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '\n':
			starts = append(starts, i+1)
		case '\r':
			if i+1 < len(src) && src[i+1] == '\n' {
				i++
			}
			starts = append(starts, i+1)
		}
	}
	return &lineIndex{src: src, starts: starts, enc: enc}
}

// position presents a byte offset, clamped into the document.
func (ix *lineIndex) position(off int) position {
	off = max(0, min(off, len(ix.src)))
	line := sort.Search(len(ix.starts), func(i int) bool { return ix.starts[i] > off }) - 1
	return position{Line: line, Character: ix.units(ix.src[ix.starts[line]:off])}
}

func (ix *lineIndex) lspRange(start, end int) lspRange {
	return lspRange{Start: ix.position(start), End: ix.position(end)}
}

func (ix *lineIndex) units(text []byte) int {
	n := 0
	for len(text) > 0 {
		r, size := utf8.DecodeRune(text)
		n += ix.width(r, size)
		text = text[size:]
	}
	return n
}

func (ix *lineIndex) width(r rune, size int) int {
	switch ix.enc {
	case "utf-8":
		return size
	case "utf-32":
		return 1
	default:
		if r >= 0x10000 {
			return 2
		}
		return 1
	}
}
