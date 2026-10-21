package structural

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/aholovko/stave/internal/tfcand"
	"github.com/aholovko/stave/internal/tfsentry"
)

func shaOf(src string) string {
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:])
}

func tfBuild(t *testing.T, src string) ([]Stream, []Result, *TFLedger) {
	t.Helper()
	streams, ledger, err := BuildTFLedger("x", "pkg/x_test.go", shaOf(src), []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if ledger.File == nil || len(streams) != ledger.File.Admitted {
		t.Fatalf("%d streams for file %+v", len(streams), ledger.File)
	}
	results := make([]Result, len(streams))
	for i, s := range streams {
		results[i] = Select(s.Header, s.Events)
	}
	return streams, results, ledger
}

func tfResult(t *testing.T, src string, results []Result, stmt string) Result {
	t.Helper()
	for _, r := range results {
		if !r.OK {
			t.Fatalf("%s failed: %s at %v", r.Cand, r.Reason, *r.EventIndex)
		}
		if iv := r.Roles["CAND"].IV[0]; src[iv[0]:iv[1]] == stmt {
			return r
		}
	}
	t.Fatalf("no result for %q", stmt)
	return Result{}
}

func texts(t *testing.T, src string, r Result, role string) []string {
	t.Helper()
	got, ok := r.Roles[role]
	if !ok {
		t.Fatalf("role %s missing from %v", role, r.Roles)
	}
	if (got.Why == "structural_empty") != (len(got.IV) == 0) {
		t.Fatalf("role %s status %s with %v", role, got.Why, got.IV)
	}
	var out []string
	for _, iv := range got.IV {
		out = append(out, src[iv[0]:iv[1]])
	}
	return out
}

func wantRole(t *testing.T, src string, r Result, role string, want ...string) {
	t.Helper()
	if got := texts(t, src, r, role); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: %q, want %q", role, got, want)
	}
}

func TestTFRolesForFirstLaterAndLabelledStatements(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\n// TestA doc.\nfunc TestA(t *testing.T) {\n\tt.Error(\"first\")\n\tx := 1\n\tt.Errorf(\"later %d\", x)\nL:\n\tt.Fatal(\"labelled\")\n}\n"
	_, results, _ := tfBuild(t, src)
	if len(results) != 3 {
		t.Fatalf("results %d", len(results))
	}
	sig := "func TestA(t *testing.T)"
	body := src[strings.Index(src, "{\n\tt.Error") : strings.LastIndex(src, "}")+1]
	first := tfResult(t, src, results, "t.Error(\"first\")")
	wantRole(t, src, first, "ARGS", "\"first\"")
	wantRole(t, src, first, "PREV")
	later := tfResult(t, src, results, "t.Errorf(\"later %d\", x)")
	wantRole(t, src, later, "ARGS", "\"later %d\", x")
	wantRole(t, src, later, "PREV", "t.Error(\"first\")\n\tx := 1\n\t")
	labelled := tfResult(t, src, results, "t.Fatal(\"labelled\")")
	wantRole(t, src, labelled, "PREV")
	for _, r := range results {
		wantRole(t, src, r, "FUNC_SIG", sig)
		wantRole(t, src, r, "BIND_SIG", sig)
		wantRole(t, src, r, "BODY", body)
		wantRole(t, src, r, "ENCL_HDR")
		wantRole(t, src, r, "RUN_PREFIX")
		if len(r.Roles) != 8 || !reflect.DeepEqual(r.Runs, [][2]int{{strings.Index(src, "// TestA"), len(src) - 1}}) {
			t.Fatalf("roles %v runs %v", r.Roles, r.Runs)
		}
	}
}

func TestTFHeadersAcrossClausesLiteralsAndIncompleteHeaders(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nfunc TestH(t *testing.T) {\n\tfor i := 0; i < 2; i++ {\n\t\tswitch i {\n\t\tcase 0:\n\t\t\tfunc() {\n\t\t\t\tt.Error(\"in literal\")\n\t\t\t}()\n\t\t}\n\t}\n\tif f := func() bool { t.Error(\"in init\"); return true }; f() {\n\t}\n}\n"
	_, results, _ := tfBuild(t, src)
	in := tfResult(t, src, results, "t.Error(\"in literal\")")
	wantRole(t, src, in, "ENCL_HDR", "for i := 0; i < 2; i++ ", "switch i ", "case 0:")
	wantRole(t, src, in, "PREV")
	cond := tfResult(t, src, results, "t.Error(\"in init\")")
	wantRole(t, src, cond, "ENCL_HDR")
}

func TestTFRunPrefixFollowsNestedRunOwnersAndIIFEName(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nfunc TestR(t *testing.T) {\n\tt.Run(func() string { return \"n\" }(), func(u *testing.T) {\n\t\tu.Run(\"in\", func(v *testing.T) {\n\t\t\tv.Error(\"inner\")\n\t\t\tu.Error(\"captured\")\n\t\t})\n\t})\n}\n"
	_, results, _ := tfBuild(t, src)
	iife := "func() string { return \"n\" }()"
	inner := tfResult(t, src, results, "v.Error(\"inner\")")
	wantRole(t, src, inner, "RUN_PREFIX", iife, "\"in\"")
	wantRole(t, src, inner, "BIND_SIG", "func(v *testing.T) ")
	captured := tfResult(t, src, results, "u.Error(\"captured\")")
	wantRole(t, src, captured, "RUN_PREFIX", iife)
	wantRole(t, src, captured, "BIND_SIG", "func(u *testing.T) ")
	wantRole(t, src, captured, "PREV", "v.Error(\"inner\")\n\t\t\t")
}

func TestTFLocalCallbackAndHelperPrefixes(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nfunc TestL(t *testing.T) {\n\tcheck := func(c *testing.T) {\n\t\tc.Error(\"x\")\n\t}\n\tt.Run(\"a\", func(s *testing.T) { check(s) })\n}\n\nfunc helper(t *testing.T) {\n\tt.Run(\"h\", func(t *testing.T) { t.Error(\"y\") })\n}\n"
	_, results, ledger := tfBuild(t, src)
	local := tfResult(t, src, results, "c.Error(\"x\")")
	wantRole(t, src, local, "RUN_PREFIX")
	wantRole(t, src, local, "BIND_SIG", "func(c *testing.T) ")
	help := tfResult(t, src, results, "t.Error(\"y\")")
	wantRole(t, src, help, "RUN_PREFIX", "\"h\"")
	ends := map[string][]string{}
	for _, row := range ledger.Rows {
		if row.Cand == "" {
			continue
		}
		if row.Context == nil || row.Context.Status != "derived" {
			t.Fatalf("%s context %+v", row.Cand, row.Context)
		}
		for _, p := range row.Context.Context.ReportedName.Paths {
			ends[row.Cand] = append(ends[row.Cand], p.End)
		}
	}
	if !reflect.DeepEqual(ends, map[string][]string{"TF:x:0": {"in_packet"}, "TF:x:1": {"caller_supplied"}}) {
		t.Fatalf("path ends %v", ends)
	}
}

func TestTFFailureCallLiteralKeepsCoverAndBodyOnlyDepth(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {\n\tt.Error(func() string {\n\t\treturn \"m\"\n\t}())\n}\n"
	_, results, _ := tfBuild(t, src)
	r := tfResult(t, src, results, "t.Error(func() string {\n\t\treturn \"m\"\n\t}())")
	wantRole(t, src, r, "ARGS", "func() string {\n\t\treturn \"m\"\n\t}()")
	lit := func(at int) int {
		for _, f := range r.Facts {
			if f.S <= at && at < f.T {
				return f.Lit
			}
		}
		t.Fatalf("byte %d not covered", at)
		return -1
	}
	if lit(strings.Index(src, "func() string")) != 0 || lit(strings.Index(src, "string {")) != 0 ||
		lit(strings.Index(src, "return")) != 1 || lit(strings.Index(src, "t.Error")) != 0 {
		t.Fatalf("literal depth facts %v", r.Facts)
	}
}

func TestTFDocCRLFUnicodeAndRootBinderNormalization(t *testing.T) {
	src := "package p\r\n\r\nimport \"testing\"\r\n\r\n// Tést doc\r\n// more\r\nfunc TestU(t *testing.T) {\r\n\tt.Errorf(`é\r\nraw`, \"ü\")\r\n}\r\n"
	streams, results, ledger := tfBuild(t, src)
	r := tfResult(t, src, results, "t.Errorf(`é\r\nraw`, \"ü\")")
	root := [2]int{strings.Index(src, "// Tést"), strings.LastIndex(src, "}") + 1}
	if !reflect.DeepEqual(r.Runs, [][2]int{root}) {
		t.Fatalf("runs %v, want %v", r.Runs, root)
	}
	var bind *Event
	for i, e := range streams[0].Events {
		if e.E == "MARK" && e.K == "CandBind" {
			bind = &streams[0].Events[i]
		}
	}
	if bind == nil || [2]int{bind.S, bind.T} != root {
		t.Fatalf("CandBind %+v, want the doc-inclusive root %v", bind, root)
	}
	row := ledger.Rows[0]
	if row.Binding.Owner.Span.Start != strings.Index(src, "func TestU") {
		t.Fatalf("the binding owner span excludes the doc comment: %+v", row.Binding.Owner.Span)
	}
	if row.Target.Start != r.Roles["CAND"].IV[0][0] || row.Target.End != r.Roles["CAND"].IV[0][1] {
		t.Fatalf("CAND %v differs from the row target %+v", r.Roles["CAND"].IV, row.Target)
	}
	wantRole(t, src, r, "FUNC_SIG", "func TestU(t *testing.T)")
	wantRole(t, src, r, "ARGS", "`é\r\nraw`, \"ü\"")
}

func TestTFLedgerIdentityAndPathGuards(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { t.Error(\"a\") }\n"
	for _, sha := range []string{"", strings.ToUpper(shaOf(src)), shaOf(src + " ")} {
		if _, _, err := BuildTFLedger("x", "x_test.go", sha, []byte(src)); err == nil {
			t.Fatalf("sha %q accepted", sha)
		}
	}
	if _, _, err := BuildTFLedger("x", "", shaOf(src), []byte(src)); err == nil {
		t.Fatal("empty path accepted")
	}
	for path, reason := range map[string]string{
		"pkg/x.go": "not_test_file", "vendor/a/x_test.go": "vendor", "a/testdata/x_test.go": "testdata",
	} {
		streams, ledger, err := BuildTFLedger("x", path, shaOf(src), []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if len(streams) != 0 || ledger.File != nil || len(ledger.Rows) != 0 || ledger.Gate.Status != "excluded" ||
			ledger.Gate.Reason != reason {
			t.Fatalf("%s: %+v", path, ledger)
		}
		h := ledger.Sentry.Hits
		if ledger.Sentry.Total != 1 || ledger.Sentry.Unresolved != 1 || h[0].Status != "unknown" ||
			h[0].Reason != "path_excluded" || h[0].Row != nil {
			t.Fatalf("%s sentry %+v", path, ledger.Sentry)
		}
	}
	_, ledger, err := BuildTFLedger("x", "notvendor/x_test.go", shaOf(src), []byte(src))
	if err != nil || ledger.Gate.Status != "eligible" || ledger.Source != (LedgerSource{ID: "x", Bytes: len(src), SHA256: shaOf(src)}) {
		t.Fatalf("eligible ledger %+v %v", ledger, err)
	}
}

// Path gating must agree across hosts, including paths with Windows separators.
func TestTFGateAcceptsBackslashPaths(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { t.Error(\"a\") }\n"
	for path, want := range map[string]TFGate{
		`C:\w\vendor\x_test.go`:   {Status: "excluded", Reason: "vendor"},
		`C:\w\testdata\x_test.go`: {Status: "excluded", Reason: "testdata"},
		`C:\w\pkg\x_test.go`:      {Status: "eligible"},
	} {
		_, ledger, err := BuildTFLedger("x", path, shaOf(src), []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if ledger.Gate != want || ledger.Path != path {
			t.Errorf("%s: gate %+v path %q, want %+v and the path as given", path, ledger.Gate, ledger.Path, want)
		}
	}
}

func TestTFParenthesizedFailureCallIsOneStreamAndOneSentryRow(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nfunc TestP(t *testing.T) {\n\t(t.Error)(\"x\")\n}\n"
	streams, results, ledger := tfBuild(t, src)
	if len(streams) != 1 {
		t.Fatalf("streams %d", len(streams))
	}
	r := tfResult(t, src, results, "(t.Error)(\"x\")")
	wantRole(t, src, r, "ARGS", "\"x\"")
	row := ledger.Rows
	if len(row) != 1 || row[0].Unit != "call" || row[0].Cand != "TF:x:0" || row[0].Context == nil ||
		row[0].Context.Status != "derived" {
		t.Fatalf("rows %+v", row)
	}
	s := ledger.Sentry
	if s.Total != 1 || s.Matched != 1 || s.Unresolved != 0 || s.Hits[0].Row == nil || *s.Hits[0].Row != 0 ||
		s.Hits[0].Offset != strings.Index(src, "Error)") {
		t.Fatalf("sentry %+v", s)
	}
}

func TestTFLedgerKeepsUnmatchedHitsUnresolved(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T, e error) {\n\tt.Error(\"a\")\n\t(t.Errorf)(\"paren\")\n\tm := (t.Fatal)\n\t_ = m\n\tg := e.Error\n\t_ = g\n\tt.Log(\"x\")\n}\n"
	streams, results, ledger := tfBuild(t, src)
	if len(streams) != 2 || !results[0].OK || !results[1].OK {
		t.Fatalf("streams %d", len(streams))
	}
	s := ledger.Sentry
	if s.Total != 4 || s.Matched != 3 || s.Unresolved != 1 {
		t.Fatalf("sentry %+v", s)
	}
	for _, h := range s.Hits {
		text := src[h.Offset:h.End]
		switch {
		case h.Offset == strings.Index(src, "Error(\"a\")"):
			if h.Row == nil || h.Status != "row" || ledger.Rows[*h.Row].Cand != "TF:x:0" {
				t.Fatalf("admitted hit %+v", h)
			}
		case h.Offset == strings.Index(src, "Errorf)"):
			if h.Row == nil || h.Status != "row" || ledger.Rows[*h.Row].Cand != "TF:x:1" {
				t.Fatalf("parenthesized call hit %+v", h)
			}
		case h.Offset == strings.Index(src, "Fatal)"):
			if h.Row == nil || *ledger.Rows[*h.Row].Reason != "method_value" {
				t.Fatalf("method value hit %+v", h)
			}
		default:
			if text != "Error" || h.Row != nil || h.Status != "unknown" || h.Reason != "no_row" || h.Form != "other" {
				t.Fatalf("unmatched hit %+v", h)
			}
		}
	}
	for _, row := range ledger.Rows {
		if (row.Cand != "") != row.Admitted || (row.Context != nil) != row.Admitted {
			t.Fatalf("row %+v", row)
		}
	}
	raw, err := json.Marshal(ledger)
	if err != nil || !strings.Contains(string(raw), `"schema":"stave-tf-ledger-1"`) || strings.Contains(string(raw), `"Sel"`) {
		t.Fatalf("ledger JSON %s %v", raw, err)
	}
}

func TestTFLedgerClassifiesHitsOnFailedFiles(t *testing.T) {
	for status, src := range map[string]string{
		"parse_error":  "package p\n\nfunc TestA(t *testing.T) {\n\tt.Error(\"a\")\n",
		"invalid_utf8": "package p\n// \xff\nfunc TestA(t *testing.T) { t.Fatal(\"a\") }\n",
		"generated":    "// Code generated by x. DO NOT EDIT.\n\npackage p\n\nfunc TestA(t *testing.T) { t.Fatal(\"a\") }\n",
	} {
		streams, ledger, err := BuildTFLedger("x", "x_test.go", shaOf(src), []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		h := ledger.Sentry.Hits
		if len(streams) != 0 || ledger.File.Status != status || len(h) != 1 || h[0].Form != "direct_call" ||
			h[0].Status != "unknown" || h[0].Reason != "file_"+status || ledger.Sentry.Unresolved != 1 {
			t.Fatalf("%s: %+v", status, ledger)
		}
	}
}

func TestTFReconcileStopsOnUnaccountedJoins(t *testing.T) {
	scan := tfsentry.Result{Hits: []tfsentry.Hit{{Period: 1, Offset: 2, End: 7, Method: "Error", Form: "direct_call"}}, Total: 1, Errors: []string{}}
	rows := []tfcand.CallRow{{Method: "Error", Sel: 2}, {Method: "Error", Sel: 2}}
	if _, err := reconcile("checked", rows, scan); err == nil {
		t.Fatal("two rows on one hit accepted")
	}
	if _, err := reconcile("checked", []tfcand.CallRow{{Method: "Fatal", Sel: 9}}, scan); err == nil {
		t.Fatal("target row without a hit accepted")
	}
	if _, err := reconcile("checked", nil, tfsentry.Result{Hits: scan.Hits, Total: 2}); err == nil {
		t.Fatal("incomplete hit enumeration accepted")
	}
	s, err := reconcile("checked", []tfcand.CallRow{{Method: "Log", Sel: 2}}, scan)
	if err != nil || s.Unresolved != 1 || s.Hits[0].Reason != "no_row" {
		t.Fatalf("non-target rows join no hit: %+v %v", s, err)
	}
}
