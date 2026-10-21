package tfcand

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

type tfExamples struct {
	TF struct {
		PresentedID string `json:"presented_id"`
		Fixtures    map[string]struct {
			Declaration string          `json:"declaration"`
			Target      string          `json:"target"`
			Context     json.RawMessage `json:"context"`
		} `json:"fixtures"`
	} `json:"tf"`
}

func loadExamples(t *testing.T) tfExamples {
	t.Helper()
	raw, err := os.ReadFile("testdata/examples.json")
	if err != nil {
		t.Fatal(err)
	}
	var ex tfExamples
	if err := json.Unmarshal(raw, &ex); err != nil {
		t.Fatal(err)
	}
	return ex
}

// span returns the unique byte range of part in decl.
func span(t *testing.T, decl, part string) []int {
	t.Helper()
	if strings.Count(decl, part) != 1 {
		t.Fatalf("%q occurs %d times", part, strings.Count(decl, part))
	}
	i := strings.Index(decl, part)
	return []int{i, i + len(part)}
}

// nth returns the byte range of the n-th (zero-based) occurrence of part in decl.
func nth(t *testing.T, decl, part string, n int) []int {
	t.Helper()
	from := 0
	for k := 0; ; k++ {
		i := strings.Index(decl[from:], part)
		if i < 0 {
			t.Fatalf("%q has no occurrence %d", part, n)
		}
		if k == n {
			return []int{from + i, from + i + len(part)}
		}
		from += i + 1
	}
}

func derive(t *testing.T, decl, target string) ContextResult {
	t.Helper()
	return DeriveContext(ContextRequest{
		Declaration: decl, Target: span(t, decl, target), ID: "t0123456789ab", TestingImports: []string{"testing"},
	})
}

func derived(t *testing.T, r ContextResult) *Context {
	t.Helper()
	if r.Status != "derived" || r.Context == nil {
		t.Fatalf("want derived, got %+v", r)
	}
	return r.Context
}

func underivable(t *testing.T, r ContextResult, reason string) {
	t.Helper()
	if r.Status != "attribute_underivable" || r.Reason != reason || r.Context != nil {
		t.Fatalf("want attribute_underivable %q, got %+v", reason, r)
	}
}

func asJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDeriveContextMatchesEveryAuthoredGoldenContext(t *testing.T) {
	ex := loadExamples(t)
	names := []string{"T1", "T2", "T3", "T4", "T5", "T6", "T7", "T8", "D1", "D2", "D3", "D4"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			f := ex.TF.Fixtures[name]
			r := DeriveContext(ContextRequest{
				Declaration: f.Declaration, Target: span(t, f.Declaration, f.Target), ID: ex.TF.PresentedID,
				TestingImports: []string{"testing"},
			})
			got := asJSON(t, derived(t, r))
			var want any
			if err := json.Unmarshal(f.Context, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				g, _ := json.Marshal(got)
				t.Fatalf("context differs\n got %s\nwant %s", g, f.Context)
			}
		})
	}
}

func TestContextKeysFollowTheContractOrder(t *testing.T) {
	ex := loadExamples(t)
	f := ex.TF.Fixtures["D1"]
	c := derived(t, derive(t, f.Declaration, f.Target))
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{`"id"`, `"caller_context"`, `"context_reasons"`, `"callback"`, `"literals"`, `"reached_literals"`,
		`"enclosing_run_name"`, `"reported_name"`, `"target_declaration_bytes"`}
	last := -1
	for _, k := range keys {
		i := bytes.Index(raw, []byte(k))
		if i <= last {
			t.Fatalf("key %s out of order in %s", k, raw)
		}
		last = i
	}
	lit := `{"position":"other","invocation":"local_visible","owns_binding":true,"declaration_bytes":[40,114],"call_sites":[[182,193]]}`
	if !bytes.Contains(raw, []byte(lit)) {
		t.Fatalf("literal key order in %s", raw)
	}
}

func TestFailNowExampleIsNotAnAdmittedTarget(t *testing.T) {
	ex := loadExamples(t)
	f := ex.TF.Fixtures["E1"]
	underivable(t, derive(t, f.Declaration, f.Target), "target_not_admitted")
}

func TestEveryLocalCallSiteGivesOnePathInSourceOrder(t *testing.T) {
	decl := "func TestMulti(t *testing.T) {\n\tcheck := func(t *testing.T) {\n\t\tt.Errorf(\"bad %d\", 1)\n\t}\n\tcheck(t)\n\tt.Run(\"a\", func(st *testing.T) { check(st) })\n}"
	c := derived(t, derive(t, decl, "t.Errorf(\"bad %d\", 1)"))
	lit := span(t, decl, "func(t *testing.T) {\n\t\tt.Errorf(\"bad %d\", 1)\n\t}")
	want := []ContextLiteral{{
		Position: "other", Invocation: "local_visible", OwnsBinding: ptrBool(true), DeclarationBytes: lit,
		CallSites: [][]int{span(t, decl, "check(t)"), span(t, decl, "check(st)")},
	}}
	if !reflect.DeepEqual(c.Literals, want) {
		t.Fatalf("literals %s", asJSONString(t, c.Literals))
	}
	run := span(t, decl, "func(st *testing.T) { check(st) }")
	if !reflect.DeepEqual(c.ReachedLiterals, []ContextLiteral{{Position: "run_argument", Invocation: "run_argument", DeclarationBytes: run}}) {
		t.Fatalf("reached %s", asJSONString(t, c.ReachedLiterals))
	}
	outer := []int{15, 16}
	paths := []ReportedPath{
		{End: "in_packet", EndBytes: outer, RunNames: []PathRunName{}},
		{End: "in_packet", EndBytes: outer, RunNames: []PathRunName{{Kind: "literal", Value: ptr("a"), DeclarationBytes: span(t, decl, "\"a\"")}}},
	}
	if !reflect.DeepEqual(c.ReportedName.Paths, paths) {
		t.Fatalf("paths %s", asJSONString(t, c.ReportedName.Paths))
	}
}

func TestNestedRunNamesAreOutermostFirst(t *testing.T) {
	decl := "func TestN(t *testing.T) {\n\tt.Run(\"outer\", func(t *testing.T) {\n\t\tt.Run(name, func(t *testing.T) {\n\t\t\tt.Error(\"x\")\n\t\t})\n\t})\n}"
	c := derived(t, derive(t, decl, "t.Error(\"x\")"))
	want := []ReportedPath{{End: "in_packet", EndBytes: []int{11, 12}, RunNames: []PathRunName{
		{Kind: "literal", Value: ptr("outer"), DeclarationBytes: span(t, decl, "\"outer\"")},
		{Kind: "dynamic", Expression: ptr("name"), DeclarationBytes: span(t, decl, "name")},
	}}}
	if !reflect.DeepEqual(c.ReportedName.Paths, want) {
		t.Fatalf("paths %s", asJSONString(t, c.ReportedName.Paths))
	}
	if c.EnclosingRunName.Kind != "dynamic" || *c.EnclosingRunName.Expression != "name" {
		t.Fatalf("frozen innermost name %+v", c.EnclosingRunName)
	}
}

func TestLiteralInvocationClassification(t *testing.T) {
	body := "func(t *testing.T) {\n\t\tt.Errorf(\"x\")\n\t}"
	cases := []struct{ name, decl, invocation, end string }{
		{"mutated", "func TestM(t *testing.T) {\n\tcheck := " + body + "\n\tcheck(t)\n\tcheck = nil\n}", "escaping", "caller_supplied"},
		{"address", "func TestA(t *testing.T) {\n\tcheck := " + body + "\n\tcheck(t)\n\t_ = &check\n}", "escaping", "caller_supplied"},
		{"argument", "func TestG(t *testing.T) {\n\tcheck := " + body + "\n\tcheck(t)\n\tuse(check)\n}", "escaping", "caller_supplied"},
		{"multiple", "func TestP(t *testing.T) {\n\tcheck, n := " + body + ", 1\n\tcheck(t)\n\t_ = n\n}", "escaping", "caller_supplied"},
		{"immediate", "func TestI(t *testing.T) {\n\t" + body + "(t)\n}", "escaping", "caller_supplied"},
		{"returned", "func mk() func(*testing.T) {\n\treturn " + body + "\n}", "escaping", "caller_supplied"},
		{"var", "func TestV(t *testing.T) {\n\tvar check = " + body + "\n\tcheck(t)\n}", "local_visible", "in_packet"},
		{"typed var", "func TestW(t *testing.T) {\n\tvar check func(*testing.T) = " + body + "\n\tcheck(t)\n}", "local_visible", "in_packet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := derived(t, derive(t, tc.decl, "t.Errorf(\"x\")"))
			if len(c.Literals) != 1 || c.Literals[0].Invocation != tc.invocation || !*c.Literals[0].OwnsBinding {
				t.Fatalf("literals %s", asJSONString(t, c.Literals))
			}
			if (tc.invocation == "local_visible") != (c.Literals[0].CallSites != nil) {
				t.Fatalf("call sites %v", c.Literals[0].CallSites)
			}
			lit := span(t, tc.decl, body)
			param := []int{lit[0] + 5, lit[0] + 6}
			p := c.ReportedName.Paths
			if len(p) != 1 || p[0].End != tc.end {
				t.Fatalf("paths %s", asJSONString(t, p))
			}
			if tc.end == "caller_supplied" && !reflect.DeepEqual(p[0].EndBytes, param) {
				t.Fatalf("end bytes %v, want literal parameter %v", p[0].EndBytes, param)
			}
		})
	}
	returned := derived(t, derive(t, cases[5].decl, "t.Errorf(\"x\")"))
	if returned.Literals[0].Position != "returned" {
		t.Fatalf("frozen position %q", returned.Literals[0].Position)
	}
}

func TestUnresolvedCallSiteArgumentEndsCallerSuppliedAtIt(t *testing.T) {
	decl := "func TestU(t *testing.T) {\n\tcheck := func(t *testing.T) {\n\t\tt.Errorf(\"x\")\n\t}\n\tcheck(t)\n\tcheck(other())\n}"
	c := derived(t, derive(t, decl, "t.Errorf(\"x\")"))
	want := []ReportedPath{
		{End: "in_packet", EndBytes: []int{11, 12}, RunNames: []PathRunName{}},
		{End: "caller_supplied", EndBytes: span(t, decl, "other()"), RunNames: []PathRunName{}},
	}
	if !reflect.DeepEqual(c.ReportedName.Paths, want) {
		t.Fatalf("paths %s", asJSONString(t, c.ReportedName.Paths))
	}
}

func TestPathsThroughNestedLocalLiterals(t *testing.T) {
	decl := "func TestL(t *testing.T) {\n\tinner := func(t *testing.T) {\n\t\tt.Errorf(\"x\")\n\t}\n\touter := func(u *testing.T) {\n\t\tinner(u)\n\t}\n\touter(t)\n}"
	c := derived(t, derive(t, decl, "t.Errorf(\"x\")"))
	outer := span(t, decl, "func(u *testing.T) {\n\t\tinner(u)\n\t}")
	want := []ContextLiteral{{Position: "other", Invocation: "local_visible", DeclarationBytes: outer, CallSites: [][]int{span(t, decl, "outer(t)")}}}
	if !reflect.DeepEqual(c.ReachedLiterals, want) {
		t.Fatalf("reached %s", asJSONString(t, c.ReachedLiterals))
	}
	if p := c.ReportedName.Paths; len(p) != 1 || p[0].End != "in_packet" || !reflect.DeepEqual(p[0].EndBytes, []int{11, 12}) {
		t.Fatalf("paths %s", asJSONString(t, p))
	}
}

func TestPathCapIsSixteen(t *testing.T) {
	build := func(n int) string {
		return "func TestC(t *testing.T) {\n\tcheck := func(t *testing.T) {\n\t\tt.Errorf(\"x\")\n\t}\n" +
			strings.Repeat("\tcheck(t)\n", n) + "}"
	}
	if c := derived(t, derive(t, build(16), "t.Errorf(\"x\")")); len(c.ReportedName.Paths) != 16 {
		t.Fatalf("paths %d", len(c.ReportedName.Paths))
	}
	underivable(t, derive(t, build(17), "t.Errorf(\"x\")"), "path_cap")
}

func TestUnderivableCases(t *testing.T) {
	ok := "func TestA(t *testing.T) {\n\tt.Error(\"x\")\n}"
	good := span(t, ok, "t.Error(\"x\")")
	cases := []struct {
		name   string
		req    ContextRequest
		reason string
	}{
		{"parse", ContextRequest{Declaration: "func TestA(t *testing.T) {\n\tt.Error(\"x\")\n", Target: good}, "parse_error"},
		{"two declarations", ContextRequest{Declaration: ok + "\nfunc f() {}", Target: good}, "span_mismatch"},
		{"not a function", ContextRequest{Declaration: "var x = 1", Target: []int{0, 3}}, "span_mismatch"},
		{"trailing comment", ContextRequest{Declaration: ok + " // c", Target: good}, "span_mismatch"},
		{"target shifted", ContextRequest{Declaration: ok, Target: []int{good[0] + 1, good[1]}}, "target_not_admitted"},
		{"target out of range", ContextRequest{Declaration: ok, Target: []int{good[0], 999}}, "span_mismatch"},
		{"dot import", ContextRequest{Declaration: ok, Target: good, DotImport: true}, "dot_import"},
		{"no testing import", ContextRequest{Declaration: ok, Target: good, TestingImports: []string{}}, "target_not_admitted"},
		{"zero call sites", ContextRequest{
			Declaration: "func TestZ(t *testing.T) {\n\tcheck := func(t *testing.T) {\n\t\tt.Error(\"x\")\n\t}\n\t_ = 0\n}",
		}, "zero_call_sites"},
		{"missing argument", ContextRequest{
			Declaration: "func TestM(t *testing.T) {\n\tcheck := func(t *testing.T) {\n\t\tt.Error(\"x\")\n\t}\n\tcheck()\n}",
		}, "call_site_argument_missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			if req.Target == nil {
				req.Target = span(t, req.Declaration, "t.Error(\"x\")")
			}
			if req.TestingImports == nil {
				req.TestingImports = []string{"testing"}
			}
			req.ID = "t0123456789ab"
			underivable(t, DeriveContext(req), tc.reason)
		})
	}
}

func TestAliasedTestingImportNameIsUsed(t *testing.T) {
	decl := "func TestA(t *tt.T) {\n\tt.Error(\"x\")\n}"
	r := DeriveContext(ContextRequest{Declaration: decl, Target: span(t, decl, "t.Error(\"x\")"), ID: "t0123456789ab", TestingImports: []string{"tt"}})
	if c := derived(t, r); c.ReportedName.Paths[0].End != "in_packet" {
		t.Fatalf("paths %s", asJSONString(t, c.ReportedName.Paths))
	}
	underivable(t, derive(t, decl, "t.Error(\"x\")"), "target_not_admitted")
}

func TestCRLFAndUnicodeOffsetsAreDeclarationBytes(t *testing.T) {
	decl := "// Tést doc\r\nfunc TestU(t *testing.T) {\r\n\tt.Run(\"é\", func(t *testing.T) {\r\n\t\tt.Errorf(`ü\r\nraw`)\r\n\t})\r\n}"
	target := "t.Errorf(`ü\r\nraw`)"
	c := derived(t, derive(t, decl, target))
	lit := span(t, decl, "func(t *testing.T) {\r\n\t\tt.Errorf(`ü\r\nraw`)\r\n\t}")
	if !reflect.DeepEqual(c.TargetDeclarationBytes, span(t, decl, target)) || !reflect.DeepEqual(c.Literals[0].DeclarationBytes, lit) {
		t.Fatalf("offsets %s", asJSONString(t, c))
	}
	outer := nth(t, decl, "t *testing.T", 0)
	want := []ReportedPath{{End: "in_packet", EndBytes: []int{outer[0], outer[0] + 1}, RunNames: []PathRunName{
		{Kind: "literal", Value: ptr("é"), DeclarationBytes: span(t, decl, "\"é\"")},
	}}}
	if !reflect.DeepEqual(c.ReportedName.Paths, want) || *c.EnclosingRunName.Value != "é" {
		t.Fatalf("paths %s", asJSONString(t, c))
	}
}

func ptrBool(b bool) *bool { return &b }

func asJSONString(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
