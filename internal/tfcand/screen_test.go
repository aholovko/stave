package tfcand

import (
	"encoding/json"
	"go/ast"
	"reflect"
	"strings"
	"testing"
)

func at(p *Parsed, n ast.Node) [2]int {
	return [2]int{offset(p.Fset, n.Pos()), offset(p.Fset, n.End())}
}

func TestScreenJoinsEveryAdmittedRowToItsStatementAndOwner(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\n// TestA doc.\nfunc TestA(t *testing.T) {\n\tt.Error(\"a\")\n\tt.Run(\"n\", func(u *testing.T) {\n\t\tt.Errorf(\"outer %d\", 1)\n\t\tu.Fatal(\"inner\")\n\t})\n\tt.Log(\"x\")\n}\n"
	p := Screen("/x_test.go", []byte(src))
	if len(p.Admitted) != 3 || p.Row.Admitted != 3 {
		t.Fatalf("admitted %d, file %d", len(p.Admitted), p.Row.Admitted)
	}
	owners := map[string]string{
		"t.Error(\"a\")":            "func TestA",
		"t.Errorf(\"outer %d\", 1)": "func TestA",
		"u.Fatal(\"inner\")":        "func(u *testing.T)",
	}
	last := -1
	for _, o := range p.Admitted {
		if o.Row <= last {
			t.Fatalf("occurrences out of row order: %d after %d", o.Row, last)
		}
		last = o.Row
		row := p.Rows[o.Row]
		if !row.Admitted || o.Stmt.X != ast.Expr(o.Call) {
			t.Fatalf("row %d is not the admitted statement call", o.Row)
		}
		if s := at(&p, o.Stmt); s != [2]int{row.Target.Start, row.Target.End} {
			t.Fatalf("statement %v, row target %+v", s, row.Target)
		}
		text := src[row.Target.Start:row.Target.End]
		if want := owners[text]; !strings.HasPrefix(src[at(&p, o.Owner)[0]:], want) {
			t.Fatalf("%s: owner %q", text, src[at(&p, o.Owner)[0]:at(&p, o.Owner)[1]])
		}
		if s := at(&p, o.Owner); s != [2]int{row.Binding.Owner.Span.Start, row.Binding.Owner.Span.End} {
			t.Fatalf("%s: owner %v, row %+v", text, s, row.Binding.Owner.Span)
		}
		if o.Decl.Name.Name != "TestA" || o.Decl.Doc == nil {
			t.Fatalf("%s: declaration %s", text, o.Decl.Name.Name)
		}
	}
}

func TestScreenFailedFilesHaveNoParse(t *testing.T) {
	for _, src := range []string{"package p\n// \xff\n", "package p\nfunc f( {\n"} {
		p := Screen("/x_test.go", []byte(src))
		if p.Row.Status == "checked" || p.Fset != nil || len(p.Rows) != 0 || len(p.Admitted) != 0 {
			t.Fatalf("%q: %+v", src, p.Row)
		}
	}
}

func TestSelectorOffsetIsUnserializedProvenance(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tt. /* c */ Error(\"a\")\n\tf := t.Fatalf\n\t_ = f\n\terr.Error()\n}\n"
	p := Screen("/x_test.go", []byte(src))
	want := map[string]int{
		"t. /* c */ Error(\"a\")": strings.Index(src, "Error(\"a\")"),
		"t.Fatalf":                strings.Index(src, "Fatalf"),
		"err.Error()":             strings.Index(src, "Error()"),
	}
	if len(p.Rows) != len(want) {
		t.Fatalf("rows %d", len(p.Rows))
	}
	for _, row := range p.Rows {
		text := src[row.Target.Start:row.Target.End]
		if row.Sel != want[text] {
			t.Fatalf("%s: sel %d, want %d", text, row.Sel, want[text])
		}
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		var keys map[string]any
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatal(err)
		}
		if _, ok := keys["Sel"]; ok || len(keys) != 18 {
			t.Fatalf("serialized keys %v", keys)
		}
	}
}

func TestRunArgsAreDirectSecondArgumentsWithReceiverOwners(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\ntype s struct{}\n\nfunc (s) Run(string, func()) {}\n\nfunc TestR(t *testing.T, x s) {\n\tt.Run(func() string { return \"n\" }(), func(t *testing.T) {\n\t\tt.Run(\"in\", func(u *testing.T) {\n\t\t\tu.Error(\"x\")\n\t\t})\n\t})\n\tx.Run(\"no\", func() {})\n\tt.Cleanup(func() {})\n}\n"
	p := Screen("/x_test.go", []byte(src))
	if len(p.Admitted) != 1 {
		t.Fatalf("admitted %d", len(p.Admitted))
	}
	fd := p.Admitted[0].Decl
	args := p.RunArgs(fd)
	if len(args) != 2 {
		t.Fatalf("run arguments %d", len(args))
	}
	got := map[string][2]string{}
	for lit, ra := range args {
		s := at(&p, lit)
		name, owner := at(&p, ra.Name), at(&p, ra.Owner)
		got[src[s[0]:s[0]+18]] = [2]string{src[name[0]:name[1]], src[owner[0] : owner[0]+18]}
	}
	want := map[string][2]string{
		"func(t *testing.T)": {"func() string { return \"n\" }()", "func TestR(t *test"},
		"func(u *testing.T)": {"\"in\"", "func(t *testing.T)"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("run arguments %q", got)
	}
}
