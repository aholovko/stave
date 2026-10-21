package lsp

import (
	"strings"
	"testing"
)

// mixed uses all three LSP line terminators. Its second line holds a two-byte rune and a four-byte rune that is a
// surrogate pair in UTF-16.
const mixed = "a\r\nbé😀c\rd\ne"

func TestPositionsFollowTheLSPLineRuleInEveryEncoding(t *testing.T) {
	for _, tc := range []struct {
		needle             string
		line               int
		utf8, utf16, utf32 int
	}{
		{"a", 0, 0, 0, 0},
		{"b", 1, 0, 0, 0},
		{"é", 1, 1, 1, 1},
		{"😀", 1, 3, 2, 2},
		{"c", 1, 7, 4, 3},
		{"d", 2, 0, 0, 0},
		{"e", 3, 0, 0, 0},
	} {
		off := strings.Index(mixed, tc.needle)
		for enc, char := range map[string]int{"utf-8": tc.utf8, "utf-16": tc.utf16, "utf-32": tc.utf32} {
			ix := newLineIndex([]byte(mixed), enc)
			if got := ix.position(off); got.Line != tc.line || got.Character != char {
				t.Errorf("%s: position(%q) = %d:%d, want %d:%d", enc, tc.needle, got.Line, got.Character, tc.line, char)
			}
		}
	}
}

func TestTheEndOfTheDocumentIsAPosition(t *testing.T) {
	ix := newLineIndex([]byte(mixed), "utf-16")
	if got := ix.position(len(mixed)); got.Line != 3 || got.Character != 1 {
		t.Errorf("end of document = %d:%d, want 3:1", got.Line, got.Character)
	}
}

func TestTheEncodingIsTheClientsFirstSupportedChoice(t *testing.T) {
	for _, tc := range []struct {
		offered []string
		want    string
	}{
		{nil, "utf-16"},
		{[]string{}, "utf-16"},
		{[]string{"utf-8", "utf-16"}, "utf-8"},
		{[]string{"utf-32", "utf-8"}, "utf-32"},
		{[]string{"ebcdic", "utf-16", "utf-8"}, "utf-16"},
		{[]string{"ebcdic"}, "utf-16"},
	} {
		if got := chooseEncoding(tc.offered); got != tc.want {
			t.Errorf("chooseEncoding(%v) = %s, want %s", tc.offered, got, tc.want)
		}
	}
}
