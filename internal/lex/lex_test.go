package lex

import (
	"bytes"
	"testing"
)

func TestScan(t *testing.T) {
	src := []byte("package p\n\n// hi\nfunc f() int {\n\treturn 42\n}\n")
	toks, err := Scan(src)
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		kind Kind
		text string
	}
	wants := []want{
		{Keyword, "package"}, {Ident, "p"}, {Comment, "// hi"},
		{Keyword, "func"}, {Ident, "f"}, {Op, "("}, {Op, ")"}, {Ident, "int"}, {Op, "{"},
		{Keyword, "return"}, {Int, "42"}, {Op, "}"},
	}
	if len(toks) != len(wants) {
		t.Fatalf("got %d tokens, want %d: %+v", len(toks), len(wants), toks)
	}
	for i, w := range wants {
		if toks[i].Kind != w.kind || toks[i].Text != w.text {
			t.Fatalf("tok[%d] = {%s %q}, want {%s %q}", i, toks[i].Kind, toks[i].Text, w.kind, w.text)
		}
	}
	if toks[1].Off != 8 || toks[3].Off != bytes.Index(src, []byte("func")) {
		t.Fatalf("byte offsets wrong: p@%d func@%d", toks[1].Off, toks[3].Off)
	}
}

func TestScanIgnoresLineDirectivesForByteOffsets(t *testing.T) {
	src := []byte("package p\n\n//line virtual.go:100\nvar Physical = 1\n")
	tokens, err := Scan(src)
	if err != nil {
		t.Fatal(err)
	}
	wantOffset := bytes.Index(src, []byte("Physical"))
	for _, token := range tokens {
		if token.Text != "Physical" {
			continue
		}
		if token.Off != wantOffset {
			t.Fatalf("Physical at byte offset %d, want %d", token.Off, wantOffset)
		}
		return
	}
	t.Fatal("Physical token not found")
}

func TestScanClassifiesEveryTokenKind(t *testing.T) {
	src := []byte("package p\nvar integer = 1\nvar floating = 1.5\nvar imaginary = 2i\nvar character = 'x'\nvar text = \"s\"\n// comment\n")
	tokens, err := Scan(src)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]Kind{}
	for _, token := range tokens {
		got[token.Text] = token.Kind
	}
	want := map[string]Kind{
		"package":    Keyword,
		"p":          Ident,
		"=":          Op,
		"1":          Int,
		"1.5":        Float,
		"2i":         Imag,
		"'x'":        Char,
		"\"s\"":      String,
		"// comment": Comment,
	}
	for text, kind := range want {
		if got[text] != kind {
			t.Errorf("token %q kind = %q, want %q", text, got[text], kind)
		}
	}

	wantKinds := []Kind{Keyword, Ident, Int, Float, Imag, Char, String, Op, Comment}
	seen := map[Kind]bool{}
	for _, kind := range got {
		seen[kind] = true
	}
	var missing []Kind
	for _, kind := range wantKinds {
		if !seen[kind] {
			missing = append(missing, kind)
		}
	}
	if len(missing) != 0 {
		t.Fatalf("missing token kinds: %v", missing)
	}
}

func TestScanRejectsLexicalErrorsWithoutPartialTokens(t *testing.T) {
	tokens, err := Scan([]byte("package p\nvar text = \"unterminated\n"))
	if err == nil {
		t.Fatal("Scan accepted an unterminated string")
	}
	if tokens != nil {
		t.Fatalf("Scan returned partial tokens after an error: %+v", tokens)
	}
}
