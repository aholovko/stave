package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/aholovko/stave/internal/diag"
	"github.com/aholovko/stave/internal/pkgsnap"
)

// testBuild pins the build selection so no test depends on the host platform.
var testBuild = pkgsnap.Build{GOOS: "linux", GOARCH: "amd64"}

// Keep fixtures as strings so formatting tools cannot rewrite deliberately unusual source.
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

func shaOf(src string) string {
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:])
}

// Find expected byte spans without a parser so parser span mistakes remain detectable.
func spanOf(t *testing.T, src, needle string, nth int) diag.Span {
	t.Helper()
	off := -1
	for i := 0; i <= nth; i++ {
		next := strings.Index(src[off+1:], needle)
		if next < 0 {
			t.Fatalf("fixture has no occurrence %d of %q", nth, needle)
		}
		off += 1 + next
	}
	return diag.Span{Start: off, End: off + len(needle)}
}

func fileReport(t *testing.T, rep diag.PackageReport, name string) diag.FileReport {
	t.Helper()
	for _, f := range rep.Files {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("report has no file %q", name)
	return diag.FileReport{}
}

// Sort findings so assertions do not depend on report order.
func found(f diag.FileReport, origin string) string {
	var items []string
	for _, d := range f.Diagnostics {
		if d.Origin == origin {
			items = append(items, fmt.Sprintf("%s@%d-%d", d.Class, d.Span.Start, d.Span.End))
		}
	}
	sort.Strings(items)
	return strings.Join(items, " ")
}

// expect renders independent expectations in the same form as found.
func expect(t *testing.T, src string, want ...[2]string) string {
	t.Helper()
	var items []string
	for _, w := range want {
		s := spanOf(t, src, w[1], 0)
		items = append(items, fmt.Sprintf("%s@%d-%d", w[0], s.Start, s.End))
	}
	sort.Strings(items)
	return strings.Join(items, " ")
}

func coverage(f diag.FileReport) string {
	var items []string
	for _, c := range f.Coverage {
		items = append(items, c.Class+" "+c.Origin+" "+c.Status)
	}
	return strings.Join(items, "; ")
}

func coverageOf(t *testing.T, f diag.FileReport, class string) diag.Coverage {
	t.Helper()
	for _, c := range f.Coverage {
		if c.Class == class {
			return c
		}
	}
	t.Fatalf("%s: no coverage entry for %s", f.Name, class)
	return diag.Coverage{}
}

// advisoryOf is coverageOf for an advisory class, which may share its stable ID with an exact class.
func advisoryOf(t *testing.T, f diag.FileReport, class string) diag.Coverage {
	t.Helper()
	for _, c := range f.Coverage {
		if c.Class == class && c.Origin == diag.OriginAdvisory {
			return c
		}
	}
	t.Fatalf("%s: no advisory coverage entry for %s", f.Name, class)
	return diag.Coverage{}
}

// Include every exposed exact class alongside other style patterns that must not
// produce findings.
const exactSource = `package demo

import (
	"fmt"
	"errors"
)

var count int = 1

var Err = errors.New("Bad thing.")

// Wrong leading word.
func DemoThing(ok bool) int {
	if ok {
		return 1
	} else {
		return 2
	}
}

var snake_case = fmt.Sprint(count)

type T struct{}

func (self T) Method() {}
`

func TestExactReportsOnlyP1P2P3(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": exactSource})

	_, rep, err := Config{Build: testBuild}.Exact(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	f := fileReport(t, rep, "demo.go")
	want := expect(t, exactSource,
		[2]string{"P1", "else"},
		[2]string{"P2", `"Bad thing."`},
		[2]string{"P3", "snake_case"},
	)
	if got := found(f, diag.OriginExact); got != want {
		t.Errorf("exact findings = %s\nwant            %s", got, want)
	}
	for _, d := range f.Diagnostics {
		if d.Message == "" {
			t.Errorf("%s has no message", d.Class)
		}
	}
}

func TestCoverageAlwaysListsEveryExactAndAdvisoryClass(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": "package demo\n"})

	_, rep, err := Config{Build: testBuild}.Exact(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	want := "P1 exact checked; P2 exact checked; P3 exact checked; " +
		"P1 advisory disabled; P2 advisory disabled; P3 advisory disabled; TF advisory disabled; DC advisory disabled; RP advisory disabled"
	if got := coverage(fileReport(t, rep, "demo.go")); got != want {
		t.Errorf("coverage = %s\nwant       %s", got, want)
	}
}

func TestIncompleteContextMakesP2UnsupportedNotNegative(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"demo.go":   exactSource,
		"broken.go": "package demo\n\nfunc {\n",
	})

	_, rep, err := Config{Build: testBuild}.Exact(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	if rep.Context.Complete || strings.Join(rep.Context.Incomplete, ";") != "broken.go: parse-error" {
		t.Errorf("context = %+v, want incomplete because of broken.go", rep.Context)
	}
	f := fileReport(t, rep, "demo.go")
	want := expect(t, exactSource, [2]string{"P1", "else"}, [2]string{"P3", "snake_case"})
	if got := found(f, diag.OriginExact); got != want {
		t.Errorf("exact findings = %s\nwant            %s", got, want)
	}
	c := coverageOf(t, f, "P2")
	if c.Status != diag.StatusUnsupported || !strings.Contains(c.Reason, "incomplete") {
		t.Errorf("P2 coverage = %s (%q), want unsupported because the context is incomplete", c.Status, c.Reason)
	}
	if len(c.Items) != 1 || !strings.Contains(c.Items[0].Reason, "broken.go") {
		t.Errorf("P2 coverage items = %+v, want the unknowable sibling named", c.Items)
	}
	for _, class := range []string{"P1", "P3"} {
		if c := coverageOf(t, f, class); c.Status != diag.StatusChecked {
			t.Errorf("%s coverage = %s, want checked", class, c.Status)
		}
	}

	broken := fileReport(t, rep, "broken.go")
	if broken.Role != diag.RoleContextOnly || len(broken.Coverage) != 0 || len(broken.Diagnostics) != 0 {
		t.Errorf("broken.go = %+v, want a context-only entry with no coverage claims", broken)
	}
}

func TestABuildInactiveSiblingDeclaringErrorsExcludesP2(t *testing.T) {
	const src = "package demo\n\nimport \"errors\"\n\nvar ErrBad = errors.New(\"Bad thing.\")\n"
	const inactive = "//go:build ignore\n\npackage main\n\nfunc errors() {}\n"

	_, alone, err := Config{Build: testBuild}.Exact(writePkg(t, map[string]string{"demo.go": src}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := found(fileReport(t, alone, "demo.go"), diag.OriginExact), expect(t, src, [2]string{"P2", `"Bad thing."`}); got != want {
		t.Fatalf("without the sibling: findings = %s, want %s", got, want)
	}

	_, shadowed, err := Config{Build: testBuild}.Exact(writePkg(t, map[string]string{"demo.go": src, "tool.go": inactive}), nil)
	if err != nil {
		t.Fatal(err)
	}
	f := fileReport(t, shadowed, "demo.go")
	if got := found(f, diag.OriginExact); got != "" {
		t.Errorf("with `errors` declared by a build-inactive sibling: findings = %s, want none", got)
	}
	if c := coverageOf(t, f, "P2"); c.Status != diag.StatusChecked {
		t.Errorf("P2 coverage = %s: a knowable shadow is a checked negative, not a gap", c.Status)
	}
	if tool := fileReport(t, shadowed, "tool.go"); tool.Role != diag.RoleContextOnly {
		t.Errorf("tool.go role = %s, want context-only", tool.Role)
	}
}

func TestAGeneratedSiblingDeclaringErrorsExcludesP2(t *testing.T) {
	const src = "package demo\n\nimport \"errors\"\n\nvar ErrBad = errors.New(\"Bad thing.\")\n"
	const generated = "// Code generated by authored-fixture. DO NOT EDIT.\n\npackage demo\n\nfunc errors() {}\n"

	_, rep, err := Config{Build: testBuild}.Exact(writePkg(t, map[string]string{"demo.go": src, "zz_generated.go": generated}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := found(fileReport(t, rep, "demo.go"), diag.OriginExact); got != "" {
		t.Errorf("with `errors` declared by a generated sibling: findings = %s, want none", got)
	}
	if g := fileReport(t, rep, "zz_generated.go"); g.Role != diag.RoleContextOnly || len(g.Diagnostics) != 0 {
		t.Errorf("zz_generated.go = %+v, want context-only and unreported", g)
	}
}

// CRLF and multibyte text exercise original-byte spans. Analyzer-only rune defaults
// and name stuttering must stay out of reports.
const crlfSource = "package demo\r\n\r\nimport \"errors\"\r\n\r\nvar r rune = 'é'\r\n\r\n" +
	"var ErrRaw = errors.New(`Bad\r\nthing.`)\r\n\r\n" +
	"var ErrPlain = errors.New(\"Worse thing\")\r\n\r\n" +
	"func DemoÉtoile(ok bool) int {\r\n\tif ok {\r\n\t\treturn 1\r\n\t} else {\r\n\t\treturn 2\r\n\t}\r\n}\r\n\r\n" +
	"var naïve_name = 1\r\n"

func TestSpansAreOriginalBytesUnderCRLFAndMultibyteText(t *testing.T) {
	want := [][2]string{
		{"P1", "else"},
		{"P2", "`Bad\r\nthing.`"},
		{"P2", `"Worse thing"`},
		{"P3", "naïve_name"},
	}
	for _, tc := range []struct{ name, src string }{
		{"CRLF", crlfSource},
		{"LF", strings.ReplaceAll(crlfSource, "\r\n", "\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rep, err := Config{Build: testBuild}.Exact(writePkg(t, map[string]string{"demo.go": tc.src}), nil)
			if err != nil {
				t.Fatal(err)
			}
			wantSet := want
			if tc.name == "LF" {
				wantSet = append([][2]string{}, want...)
				wantSet[1] = [2]string{"P2", "`Bad\nthing.`"}
			}
			if got, want := found(fileReport(t, rep, "demo.go"), diag.OriginExact), expect(t, tc.src, wantSet...); got != want {
				t.Errorf("exact findings = %s\nwant            %s", got, want)
			}
		})
	}
}

func TestSourceThatIsNotGofmtCleanIsStillAnalysed(t *testing.T) {
	const src = "package demo\nvar   count   int=1\nfunc f(ok bool) int { if ok {return 1} else {return 2} }\n"

	_, rep, err := Config{Build: testBuild}.Exact(writePkg(t, map[string]string{"demo.go": src}), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := expect(t, src, [2]string{"P1", "else"})
	if got := found(fileReport(t, rep, "demo.go"), diag.OriginExact); got != want {
		t.Errorf("exact findings = %s, want %s", got, want)
	}
}

func TestExactReportsSavedPackageP3AtTheDeclaredName(t *testing.T) {
	const src = "package demo\n\nvar snake_case = 1\n"
	dir := writePkg(t, map[string]string{"demo.go": src})

	snap, rep, err := Config{Build: testBuild}.Exact(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	f := fileReport(t, rep, "demo.go")
	if f.Role != diag.RoleTarget {
		t.Fatalf("role = %q, want target", f.Role)
	}
	if len(f.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want exactly one P3", f.Diagnostics)
	}
	d := f.Diagnostics[0]
	if d.Class != "P3" || d.Origin != diag.OriginExact {
		t.Errorf("class/origin = %s/%s, want P3/exact", d.Class, d.Origin)
	}
	if want := spanOf(t, src, "snake_case", 0); d.Span != want {
		t.Errorf("span = %+v, want %+v", d.Span, want)
	}
	if d.SourceSHA256 != shaOf(src) {
		t.Errorf("source sha = %s, want %s", d.SourceSHA256, shaOf(src))
	}
	if d.ContextDigest == "" || d.ContextDigest != snap.Context.Digest() || d.ContextDigest != rep.Context.Digest {
		t.Errorf("context digest %q does not bind the snapshot digest %q and report digest %q",
			d.ContextDigest, snap.Context.Digest(), rep.Context.Digest)
	}
	if d.Path != filepath.Join(snap.Dir, "demo.go") {
		t.Errorf("path = %q, want %q", d.Path, filepath.Join(snap.Dir, "demo.go"))
	}
	if d.Backend == nil || d.Backend.ID != "stave-exact" || d.Score != nil {
		t.Errorf("backend = %+v, score = %v; want the exact analyzer and no score", d.Backend, d.Score)
	}
}

func TestTheReportRecordsTheBuildSelectionItUsed(t *testing.T) {
	cfg := Config{Build: pkgsnap.Build{GOOS: "linux", GOARCH: "amd64", Tags: []string{"special"}}}
	_, rep, err := cfg.Exact(writePkg(t, map[string]string{"demo.go": "package demo\n"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	b := rep.Build
	if b.GOOS != "linux" || b.GOARCH != "amd64" || strings.Join(b.Tags, ",") != "special" || b.Cgo || b.Go != runtime.Version() {
		t.Errorf("build = %+v", b)
	}
	if rep.Schema != "stave-report-v1" {
		t.Errorf("schema = %q", rep.Schema)
	}
}
