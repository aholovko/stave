package structural_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aholovko/stave/internal/analyzer"
	"github.com/aholovko/stave/internal/pkgsnap"
	"github.com/aholovko/stave/internal/structural"
)

func toRecord(task string, k int, r oRec) structural.Record {
	return structural.Record{ID: fmt.Sprintf("%s:fx:%d", task, k), Status: r.status, Reasons: r.reasons,
		Anchor: r.anchor, Cand: r.cand, Envelope: r.env, Base: r.base, Name: r.name, Subtype: r.subtype,
		Import: r.imp, FileConflicts: r.fileConf, PackageConflicts: r.pkgConf}
}

func wantResult(task, id string, src []byte, r structural.Record) structural.Result {
	a, c := *r.Envelope, *r.Cand
	whole := map[string]string{"P2": "CALL", "P3": "DECL"}[task]
	return structural.Result{OK: true, Task: task, Cand: id, Source: structural.Source{Bytes: len(src), SHA256: shaHex(src)},
		Runs:  [][2]int{a},
		Facts: []structural.Fact{{S: a[0], T: a[1], Kind: "NoStmt", Parent: "Outside", Ordinal: -1, Depth: -1}},
		Roles: map[string]structural.Role{"CAND": {IV: [][2]int{c}, Why: "present"},
			whole: {IV: [][2]int{a}, Why: "present"}}}
}

func oracleFor(t *testing.T, task string, src []byte, pkg *oPkg) []structural.Record {
	t.Helper()
	var recs []oRec
	var err error
	if task == "P2" {
		recs, err = oracleP2(src, pkg)
	} else {
		recs, err = oracleP3(src)
	}
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	out := make([]structural.Record, len(recs))
	for k, r := range recs {
		out[k] = toRecord(task, k, r)
	}
	return out
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func checkAgainstOracle(t *testing.T, task string, src []byte, cert *pkgsnap.Certificate, pkg *oPkg) *structural.Ledger {
	t.Helper()
	streams, ledger, err := structural.BuildLedger(task, "fx", src, cert)
	if err != nil {
		t.Fatalf("BuildLedger: %v", err)
	}
	want := oracleFor(t, task, src, pkg)
	if g, w := mustJSON(t, ledger.Records), mustJSON(t, want); g != w {
		t.Fatalf("ledger differs from the oracle:\n got %s\nwant %s", g, w)
	}
	if ledger.Task != task || ledger.Source.ID != "fx" || ledger.Source.Bytes != len(src) || ledger.Source.SHA256 != shaHex(src) {
		t.Errorf("ledger source = %+v", ledger.Source)
	}
	k := 0
	for _, r := range ledger.Records {
		if (r.Cand == nil) != (r.Envelope == nil) {
			t.Errorf("%s: CAND and envelope must be set together", r.ID)
		}
		if r.Cand == nil {
			continue
		}
		if k >= len(streams) {
			t.Fatalf("streamed record %s has no stream", r.ID)
		}
		s := streams[k]
		k++
		if s.Header.Cand != r.ID {
			t.Errorf("stream %d header cand %q, want %q", k-1, s.Header.Cand, r.ID)
		}
		got := structural.Select(s.Header, s.Events)
		if g, w := mustJSON(t, got), mustJSON(t, wantResult(task, r.ID, src, r)); g != w {
			t.Errorf("%s: selector result\n got %s\nwant %s", r.ID, g, w)
		}
		if got.OK && got.Roles["CAND"].IV[0] != *r.Cand {
			t.Errorf("%s: ledger CAND %v differs from the CAND role %v", r.ID, *r.Cand, got.Roles["CAND"].IV)
		}
	}
	if k != len(streams) {
		t.Errorf("%d streams for %d streamed records", len(streams), k)
	}
	return ledger
}

func TestLedgerMatchesOracle(t *testing.T) {
	for _, fx := range acceptedFixtures {
		t.Run(fx.name, func(t *testing.T) {
			if _, err := parser.ParseFile(token.NewFileSet(), "", fx.src, parser.ParseComments); err != nil {
				t.Fatalf("accepted fixture does not parse under go1.26.5: %v", err)
			}
			checkAgainstOracle(t, fx.task, []byte(fx.src), nil, nil)
		})
	}
}

func TestLedgerParseErrorFixturesFail(t *testing.T) {
	for _, fx := range parseErrorFixtures {
		if _, err := parser.ParseFile(token.NewFileSet(), "", fx.src, parser.ParseComments); err == nil {
			t.Errorf("%s: parse-error fixture parses", fx.name)
		}
		if s, l, err := structural.BuildLedger(fx.task, "fx", []byte(fx.src), nil); err == nil || s != nil || l != nil {
			t.Errorf("%s: BuildLedger accepted a parse error", fx.name)
		}
	}
}

func TestLedgerHandComputedSpans(t *testing.T) {
	byName := map[string]ledgerFixture{}
	for _, fx := range acceptedFixtures {
		byName[fx.name] = fx
	}
	for _, h := range handSpans {
		src := byName[h.fixture].src
		parts := strings.Split(h.env, "|")
		if len(parts) == 1 {
			parts = []string{"", h.env, ""}
		}
		text := strings.Join(parts, "")
		if strings.Count(src, text) != 1 {
			t.Fatalf("%s: envelope %q is not unique", h.fixture, text)
		}
		a := strings.Index(src, text) + len(parts[0])
		c := a + strings.LastIndex(parts[1], h.cand)
		env, cand := [2]int{a, a + len(parts[1])}, [2]int{c, c + len(h.cand)}
		_, ledger, err := structural.BuildLedger(byName[h.fixture].task, "fx", []byte(src), nil)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range ledger.Records {
			if r.Cand != nil && *r.Cand == cand {
				found = true
				if *r.Envelope != env || r.Status != h.status || r.Subtype != h.subtype {
					t.Errorf("%s %q: envelope %v status %s subtype %s, want %v %s %s", h.fixture, h.cand,
						*r.Envelope, r.Status, r.Subtype, env, h.status, h.subtype)
				}
			}
		}
		if !found {
			t.Errorf("%s: no streamed record with CAND %q at %v", h.fixture, h.cand, cand)
		}
	}
}

func statusesOf(src string, l *structural.Ledger) map[string]string {
	out := map[string]string{}
	for _, r := range l.Records {
		out[src[r.Anchor[0]:r.Anchor[1]]+"@"+fmt.Sprint(r.Anchor[0])] = strings.Join(r.Reasons, ",")
	}
	return out
}

func recordAt(t *testing.T, src string, l *structural.Ledger, text string, nth int) structural.Record {
	t.Helper()
	at := -1
	for i := 0; i <= nth; i++ {
		j := strings.Index(src[at+1:], text)
		if j < 0 {
			t.Fatalf("%q occurrence %d not found", text, nth)
		}
		at += 1 + j
	}
	for _, r := range l.Records {
		if r.Anchor == [2]int{at, at + len(text)} {
			return r
		}
	}
	t.Fatalf("no record anchored at %q occurrence %d", text, nth)
	return structural.Record{}
}

func TestLedgerNamedStatuses(t *testing.T) {
	cases := []struct {
		fixture, anchor string
		nth             int
		reasons         string
	}{
		{"p2_callees", "\"Bad.\"", 0, "unknown_context"},
		{"p2_callees", "(errors.New)()", 0, "no_argument,unknown_context"},
		{"p2_callees", "(\"Bad.\")", 2, "indirect_argument,unknown_context"},
		{"p2_callees", "\"Bad.\"", 3, "excluded_paren_base,unknown_context"},
		{"p2_callees", "\"x\"", 0, "excluded_paren_base,unknown_context"},
		{"p2_callees", "\"x\"", 1, "excluded_paren_base,unknown_context"},
		{"p2_arguments", "msg", 1, "indirect_argument,unknown_context"},
		{"p2_arguments", "\"a\" + \"b\"", 0, "indirect_argument,unknown_context"},
		{"p2_arguments", "errors.New()", 0, "no_argument,unknown_context"},
		{"p2_import_alias", "\"x\"", 0, "excluded_import,unknown_context"},
		{"p2_import_same_alias", "\"x\"", 0, "excluded_import,unknown_context"},
		{"p2_import_blank", "\"x\"", 0, "excluded_import,unknown_context"},
		{"p2_import_dot", "\"x\"", 0, "excluded_import,unknown_context"},
		{"p2_import_order_plain_first", "\"x\"", 0, "unknown_context"},
		{"p2_import_order_alias_first", "\"x\"", 0, "excluded_import,unknown_context"},
		{"p2_import_raw_path", "\"x\"", 0, "unknown_context"},
		{"p2_import_escaped_path", "\"x\"", 0, "unknown_context"},
		{"p2_no_import", "\"x\"", 0, "excluded_import,unknown_context"},
		{"p2_no_import", "\"x\"", 1, "excluded_import,unknown_context"},
		{"p2_conflict_variable", "\"x\"", 0, "excluded_file_conflict"},
		{"p2_conflict_variable", "\"x\"", 1, "unknown_context"},
		{"p2_conflict_parameter", "\"x\"", 0, "unknown_context"},
		{"p2_conflict_parameter", "\"x\"", 1, "excluded_file_conflict"},
		{"p2_conflict_field", "\"x\"", 0, "excluded_file_conflict"},
		{"p2_conflict_label", "\"x\"", 0, "excluded_file_conflict"},
		{"p2_conflict_label", "\"x\"", 1, "unknown_context"},
		{"p2_conflict_type_param", "\"x\"", 0, "excluded_file_conflict"},
		{"p2_conflict_receiver_type_param", "\"x\"", 0, "excluded_file_conflict"},
		{"p2_conflict_receiver_type_param", "\"x\"", 1, "unknown_context"},
		{"p2_no_conflict_star_receiver_arg", "\"x\"", 0, "unknown_context"},
		{"p2_alias_other_path", "\"x\"", 0, "unknown_context"},
		{"p2_alias_other_path_local", "\"x\"", 0, "excluded_file_conflict"},
		{"p2_alias_only_local", "\"x\"", 0, "excluded_import,excluded_file_conflict"},
	}
	byName := map[string]ledgerFixture{}
	for _, fx := range acceptedFixtures {
		byName[fx.name] = fx
	}
	for _, c := range cases {
		src := byName[c.fixture].src
		_, l, err := structural.BuildLedger("P2", "fx", []byte(src), nil)
		if err != nil {
			t.Fatal(err)
		}
		r := recordAt(t, src, l, c.anchor, c.nth)
		if got := strings.Join(r.Reasons, ","); got != c.reasons || r.Status != r.Reasons[0] {
			t.Errorf("%s %q#%d: status %s reasons %s, want %s", c.fixture, c.anchor, c.nth, r.Status, got, c.reasons)
		}
		streamed := !strings.HasPrefix(c.reasons, "excluded_paren_base") && !strings.HasPrefix(c.reasons, "no_argument") &&
			!strings.HasPrefix(c.reasons, "indirect_argument")
		if (r.Cand != nil) != streamed {
			t.Errorf("%s %q#%d: streamed %v, want %v", c.fixture, c.anchor, c.nth, r.Cand != nil, streamed)
		}
	}

	// The receiver type parameter conflict cites the span of that identifier.
	src := byName["p2_conflict_receiver_type_param"].src
	_, l, _ := structural.BuildLedger("P2", "fx", []byte(src), nil)
	at := strings.Index(src, "S[errors]") + 2
	if r := recordAt(t, src, l, "\"x\"", 0); !reflect.DeepEqual(r.FileConflicts, [][2]int{{at, at + 6}}) {
		t.Errorf("file conflicts = %v, want the receiver type parameter at %d", r.FileConflicts, at)
	}
	// Uncounted P2 shapes are not enumerated at all.
	src = byName["p2_callees"].src
	_, l, _ = structural.BuildLedger("P2", "fx", []byte(src), nil)
	if len(l.Records) != 10 {
		t.Errorf("p2_callees enumerates %d records, want 10: %s", len(l.Records), mustJSON(t, statusesOf(src, l)))
	}
	// Equal anchors keep AST pre-order: the outer call before its argument.
	if n := len(l.Records); n >= 2 && (l.Records[n-2].Anchor != l.Records[n-1].Anchor ||
		l.Records[n-2].Status != "indirect_argument" || l.Records[n-1].Status != "no_argument") {
		t.Errorf("equal anchors: %+v then %+v", l.Records[n-2], l.Records[n-1])
	}
}

func TestLedgerP3ReceiverStatuses(t *testing.T) {
	byName := map[string]ledgerFixture{}
	for _, fx := range acceptedFixtures {
		byName[fx.name] = fx
	}
	src := byName["p3_receiver_exclusions"].src
	_, l, err := structural.BuildLedger("P3", "fx", []byte(src), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"*T_x", "[]int", "p.T_x", "struct{ f_x int }", "func(p_q int)", "F(x)"} {
		r := recordAt(t, src, l, arg, 0)
		if r.Status != "excluded_receiver_argument" || r.Cand != nil || r.Name != "" {
			t.Errorf("%s: %+v, want a ledger-only receiver argument", arg, r)
		}
	}
	src = byName["p3_receivers"].src
	_, l, _ = structural.BuildLedger("P3", "fx", []byte(src), nil)
	blank := recordAt(t, src, l, "_", strings.Count(src[:strings.Index(src, "S[_,")], "_"))
	if blank.Status != "excluded_blank" || blank.Cand != nil {
		t.Errorf("receiver blank = %+v", blank)
	}
	k := recordAt(t, src, l, "K_x", 0)
	v := recordAt(t, src, l, "V", 0)
	if k.Cand == nil || v.Cand == nil || *k.Envelope != *v.Envelope {
		t.Errorf("S[K_x, V] gives two streams with one envelope: %+v %+v", k, v)
	}
	src = byName["p3_receiver_nothing"].src
	_, l, _ = structural.BuildLedger("P3", "fx", []byte(src), nil)
	var names []string
	for _, r := range l.Records {
		if r.Subtype == "receiver_type_parameter" || r.Status == "excluded_receiver_argument" {
			names = append(names, r.Name)
		}
	}
	if !reflect.DeepEqual(names, []string{"A_x", "B_x"}) {
		t.Errorf("supplement records = %v, want only the two receiver fields' A_x, B_x", names)
	}
}

// Package evidence may change eligibility, but not occurrence identity.
func TestLedgerStreamsInvariantToCertificate(t *testing.T) {
	const target = p2Imports + "var _ = errors.New(\"Bad.\")\nvar _ = fmt.Errorf(\"x\")\nvar _ = (errors).New(\"y\")\n"
	clean := writePkg(t, map[string]string{"t.go": target})
	conflicting := writePkg(t, map[string]string{"t.go": target, "c.go": "package p\n\nvar errors, fmt = 1, 2\n"})
	var certs []*pkgsnap.Certificate
	certs = append(certs, nil, &pkgsnap.Certificate{})
	for _, dir := range []string{clean, conflicting} {
		c, err := pkgsnap.Certify(filepath.Join(dir, "t.go"))
		if err != nil {
			t.Fatal(err)
		}
		certs = append(certs, c)
	}
	var base string
	var statuses []string
	for i, c := range certs {
		streams, l, err := structural.BuildLedger("P2", "fx", []byte(target), c)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range l.Records {
			ids = append(ids, r.ID+fmt.Sprint(r.Anchor, r.Cand, r.Envelope))
		}
		got := mustJSON(t, ids) + mustJSON(t, streams)
		if i == 0 {
			base = got
		} else if got != base {
			t.Errorf("certificate %d changed IDs or stream bytes", i)
		}
		statuses = append(statuses, l.Records[0].Status)
	}
	if want := []string{"unknown_context", "unknown_context", "eligible", "excluded_package_conflict"}; !reflect.DeepEqual(statuses, want) {
		t.Errorf("statuses = %v, want %v", statuses, want)
	}
}

func writePkg(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func certified(t *testing.T, dir string) (*pkgsnap.Certificate, []byte) {
	t.Helper()
	path := filepath.Join(dir, "t.go")
	c, err := pkgsnap.Certify(path)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return c, src
}

const pkgTarget = p2Imports + "var _ = errors.New(\"Bad.\")\nvar _ = fmt.Errorf(\"x\")\n"

func TestLedgerPackageContexts(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		setup   func(t *testing.T, dir string)
		errs    string
		fmtWant string
	}{
		{name: "generated", files: map[string]string{"g.go": "// Code generated by fixture. DO NOT EDIT.\n\npackage p\n\nvar errors = 1\n"},
			errs: "excluded_package_conflict", fmtWant: "eligible"},
		{name: "build_excluded", files: map[string]string{"x.go": "//go:build ignore\n\npackage p\n\nfunc errors() {}\n"},
			errs: "excluded_package_conflict", fmtWant: "eligible"},
		{name: "other_package", files: map[string]string{"o.go": "package other\n\ntype errors int\n"},
			errs: "excluded_package_conflict", fmtWant: "eligible"},
		{name: "test_file_ignored", files: map[string]string{"x_test.go": "package p\n\nvar errors = 1\n"},
			errs: "eligible", fmtWant: "eligible"},
		{name: "method_and_nested_are_not_package_block", files: map[string]string{
			"m.go": "package p\n\ntype T int\n\nfunc (T) errors() {}\n\nfunc f() { var fmt = 1; _ = fmt }\n"},
			errs: "eligible", fmtWant: "eligible"},
		{name: "unreadable", files: map[string]string{"c.go": "package p\n\nvar errors = 1\n"},
			setup: func(t *testing.T, dir string) {
				if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "dangling.go")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}, errs: "excluded_package_conflict", fmtWant: "unknown_context"},
		{name: "oversized", files: map[string]string{"c.go": "package p\n\nvar errors = 1\n", "big.go": "package p\n"},
			setup: func(t *testing.T, dir string) {
				if err := os.Truncate(filepath.Join(dir, "big.go"), pkgsnap.MaxFileBytes+1); err != nil {
					t.Fatal(err)
				}
			}, errs: "excluded_package_conflict", fmtWant: "unknown_context"},
		{name: "parse_error", files: map[string]string{"c.go": "package p\n\nvar errors = 1\n", "b.go": "package p\n\nfunc {\n"},
			errs: "excluded_package_conflict", fmtWant: "unknown_context"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.files["t.go"] = pkgTarget
			dir := writePkg(t, tc.files)
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			cert, src := certified(t, dir)
			pkg := oracleReadPkg(dir)
			l := checkAgainstOracle(t, "P2", src, cert, &pkg)
			got := []string{l.Records[0].Status, l.Records[1].Status}
			if want := []string{tc.errs, tc.fmtWant}; !reflect.DeepEqual(got, want) {
				t.Errorf("statuses = %v, want %v", got, want)
			}
			if l.Certificate == nil || l.Certificate.Target != "t.go" {
				t.Errorf("ledger certificate summary = %+v", l.Certificate)
			}
		})
	}
}

func TestLedgerTargetMismatchIsIdentityError(t *testing.T) {
	dir := writePkg(t, map[string]string{"t.go": pkgTarget})
	cert, src := certified(t, dir)
	streams, l, err := structural.BuildLedger("P2", "fx", append(src, '\n'), cert)
	if !errors.Is(err, pkgsnap.ErrTargetIdentity) || streams != nil || l != nil {
		t.Errorf("mismatched target: err %v, streams %v, ledger %v", err, streams, l)
	}
}

func TestLedgerTaskAndCertificateMisuse(t *testing.T) {
	dir := writePkg(t, map[string]string{"t.go": pkgTarget})
	cert, src := certified(t, dir)
	if _, _, err := structural.BuildLedger("P3", "fx", src, cert); err == nil {
		t.Error("P3 takes no package certificate")
	}
	if _, _, err := structural.BuildLedger("P1", "fx", src, nil); err == nil {
		t.Error("BuildLedger is P2/P3 only")
	}
	if _, err := structural.Build("P2", "fx", src); err == nil {
		t.Error("P2 requires BuildLedger")
	}
	if _, _, err := structural.BuildLedger("P3", "fx", []byte("package p\n\nvar s = \"\xff\"\n"), nil); err == nil {
		t.Error("a source that is not valid UTF-8 is rejected")
	}
}

func TestLedgerLongLiteralIsNotTruncated(t *testing.T) {
	lit := "\"" + strings.Repeat("a", 64<<10) + "\""
	src := []byte(p2Imports + "var _ = errors.New(" + lit + ")\n")
	streams, l, err := structural.BuildLedger("P2", "fx", src, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := l.Records[0]
	if r.Cand == nil || r.Cand[1]-r.Cand[0] != len(lit) {
		t.Fatalf("CAND = %v, want %d bytes", r.Cand, len(lit))
	}
	if res := structural.Select(streams[0].Header, streams[0].Events); !res.OK || res.Roles["CAND"].IV[0] != *r.Cand {
		t.Errorf("result = %+v", res.Reason)
	}
}

// Base records must agree with DeclaringIdents; receiver type parameters are
// covered by the separate supplement.
func TestLedgerBaseRecordsMatchDeclaringIdents(t *testing.T) {
	for _, fx := range acceptedFixtures {
		if fx.task != "P3" {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "", fx.src, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		_, l, err := structural.BuildLedger("P3", "fx", []byte(fx.src), nil)
		if err != nil {
			t.Fatal(err)
		}
		declared := map[[2]int]bool{}
		for _, id := range analyzer.DeclaringIdents(file) {
			declared[[2]int{fset.Position(id.Pos()).Offset, fset.Position(id.End()).Offset}] = true
		}
		for _, r := range l.Records {
			supplement := r.Subtype == "receiver_type_parameter" || r.Status == "excluded_receiver_argument"
			if declared[r.Anchor] == supplement {
				t.Errorf("%s: %s %q declared=%v supplement=%v", fx.name, r.ID, r.Name, declared[r.Anchor], supplement)
			}
			delete(declared, r.Anchor)
		}
		if len(declared) != 0 {
			t.Errorf("%s: declaring identifiers without a base record: %v", fx.name, declared)
		}
	}
}
