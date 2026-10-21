package tfsentry

import (
	"reflect"
	"strings"
	"testing"
)

func hits(src string) []string {
	var out []string
	for _, h := range Scan([]byte(src)).Hits {
		out = append(out, src[h.Offset:h.End]+"@"+h.Form)
	}
	return out
}

func TestScanFindsSelectorsAcrossWhitespaceAndComments(t *testing.T) {
	src := "package p\n\nfunc f() {\n\tt. /* c */ Error(\"a\")\n\tt.\n\t\tFatalf(\"b\")\n\t(t.Errorf)(\"c\")\n\tg := t.Fatal\n\tt.Log(\"Error\")\n\t// t.Error(\"comment\")\n\t_ = \"x.Error\"\n\tx.Error\n\t(\"semicolon\")\n}\n"
	got := hits(src)
	want := []string{"Error@direct_call", "Fatalf@direct_call", "Errorf@other", "Fatal@other", "Error@other"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hits %q", got)
	}
	r := Scan([]byte(src))
	if r.Total != len(r.Hits) || len(r.Errors) != 0 {
		t.Fatalf("total %d errors %v", r.Total, r.Errors)
	}
	first := r.Hits[0]
	if src[first.Period:first.Period+1] != "." || first.Offset != strings.Index(src, "Error(\"a\")") || first.Method != "Error" {
		t.Fatalf("first hit %+v", first)
	}
}

func TestScanCountsScannerErrorsAsFileOutcomes(t *testing.T) {
	src := "package p\n// \xff\nfunc f() { t.Error(\"x\"); `unterminated\n"
	r := Scan([]byte(src))
	if len(r.Errors) == 0 {
		t.Fatal("scanner errors are retained")
	}
	if r.Total != 1 || len(r.Hits) != 1 || r.Hits[0].Method != "Error" {
		t.Fatalf("hits %+v", r)
	}
	bad := "package p\nfunc f() { t.Errorf(\"\xff\") \n"
	if r := Scan([]byte(bad)); r.Total != 1 || len(r.Errors) == 0 {
		t.Fatalf("invalid UTF-8 in a string: %+v", r)
	}
}

func TestScanIgnoresOtherIdentifiersAndKeywords(t *testing.T) {
	src := "package p\n\nimport \"errors\"\n\nvar Error = errors.New(\"x\")\n\nfunc f() { _ = Error; _ = x.ErrorF; _ = x.Errors }\n"
	if got := hits(src); got != nil {
		t.Fatalf("hits %q", got)
	}
}
