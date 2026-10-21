package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aholovko/stave/internal/advisory"
	"github.com/aholovko/stave/internal/diag"
	"github.com/aholovko/stave/internal/pkgsnap"
)

type providerFunc func(context.Context, advisory.Request) (advisory.Response, error)

func (f providerFunc) Predict(ctx context.Context, req advisory.Request) (advisory.Response, error) {
	return f(ctx, req)
}

// docSource carries one exact finding and a documented exported function whose comment is the advisory candidate. The
// é makes byte and rune offsets differ.
const docSource = "package demo\n\nvar the_count = 1\n\n// Café returns the count, obviously.\nfunc Café() int { return the_count }\n"

func ptr[T any](v T) *T { return &v }

// answer builds a well-formed DC response. Every requested file is checked, and predict supplies the predictions for
// one requested file.
func answer(predict func(f advisory.FileRef) []advisory.Prediction) providerFunc {
	return func(_ context.Context, req advisory.Request) (advisory.Response, error) {
		resp := advisory.Response{
			Schema:  advisory.Schema,
			Backend: advisory.Backend{ID: "fake-dc", Digest: "feed", Detail: "full"},
			Classes: []string{"DC"},
		}
		for _, f := range req.Files {
			resp.Files = append(resp.Files, advisory.FileRecord{Path: f.Path, SHA256: f.SHA256, Status: advisory.StatusChecked})
			resp.Predictions = append(resp.Predictions, predict(f)...)
		}
		return resp, nil
	}
}

func advised(f advisory.FileRef, s diag.Span, advice bool, score float64) advisory.Prediction {
	return advisory.Prediction{
		Path: f.Path, Class: "DC", SHA256: f.SHA256, Span: s,
		Coverage: advisory.StatusChecked, Advice: &advice, Score: &score,
	}
}

func run(t *testing.T, src string, provider advisory.Provider) diag.PackageReport {
	t.Helper()
	cfg := Config{Build: testBuild, Provider: provider}
	snap, rep, err := cfg.Exact(writePkg(t, map[string]string{"demo.go": src}), nil)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Advise(context.Background(), []*pkgsnap.Snapshot{snap}, []diag.PackageReport{rep})[0]
}

// exactView renders everything that the exact path contributed to a file. This must not depend on the advisory
// provider in any way.
func exactView(f diag.FileReport) string {
	var cov []string
	for _, c := range f.Coverage {
		if c.Origin == diag.OriginExact {
			cov = append(cov, c.Class+" "+c.Status+" "+c.Reason)
		}
	}
	return found(f, diag.OriginExact) + " | " + strings.Join(cov, "; ")
}

func TestAdviseWithoutProviderStatesTheConfiguredReason(t *testing.T) {
	const reason = "no model installed; run stave setup"
	cfg := Config{Build: testBuild, Disabled: reason}
	snap, rep, err := cfg.Exact(writePkg(t, map[string]string{"demo.go": docSource}), nil)
	if err != nil {
		t.Fatal(err)
	}

	rep = cfg.Advise(context.Background(), []*pkgsnap.Snapshot{snap}, []diag.PackageReport{rep})[0]

	if rep.Advisory.Status != diag.StatusDisabled || rep.Advisory.Reason != reason {
		t.Errorf("advisory = %+v, want disabled with the configured reason", rep.Advisory)
	}
	f := fileReport(t, rep, "demo.go")
	for _, class := range diag.AdvisoryClasses {
		if c := advisoryOf(t, f, class); c.Status != diag.StatusDisabled || c.Reason != reason {
			t.Errorf("%s coverage = %s (%q), want disabled with the configured reason", class, c.Status, c.Reason)
		}
	}
}

func TestNoProviderIsDisabledNotNegative(t *testing.T) {
	rep := run(t, docSource, nil)

	if rep.Advisory.Provider != "disabled" || rep.Advisory.Status != diag.StatusDisabled {
		t.Errorf("advisory = %+v, want the disabled provider", rep.Advisory)
	}
	f := fileReport(t, rep, "demo.go")
	for _, class := range diag.AdvisoryClasses {
		if c := advisoryOf(t, f, class); c.Status != diag.StatusDisabled || c.Reason == "" {
			t.Errorf("%s coverage = %s (%q), want disabled with a reason", class, c.Status, c.Reason)
		}
	}
	if got := found(f, diag.OriginAdvisory); got != "" {
		t.Errorf("advisory findings = %s, want none", got)
	}
}

func TestCheckedAdviceBecomesAnAdvisoryDiagnosticBoundToTheSource(t *testing.T) {
	comment := spanOf(t, docSource, "// Café returns the count, obviously.", 0)
	rep := run(t, docSource, answer(func(f advisory.FileRef) []advisory.Prediction {
		return []advisory.Prediction{advised(f, comment, true, 0.8)}
	}))

	if rep.Advisory.Status != "ok" || rep.Advisory.Backend == nil || rep.Advisory.Backend.ID != "fake-dc" {
		t.Errorf("advisory = %+v, want ok from fake-dc", rep.Advisory)
	}
	if strings.Join(rep.Advisory.Classes, ",") != "DC" {
		t.Errorf("advisory classes = %v, want the classes the backend declared", rep.Advisory.Classes)
	}
	f := fileReport(t, rep, "demo.go")
	var got []diag.Diagnostic
	for _, d := range f.Diagnostics {
		if d.Origin == diag.OriginAdvisory {
			got = append(got, d)
		}
	}
	if len(got) != 1 {
		t.Fatalf("advisory diagnostics = %+v, want one", got)
	}
	d := got[0]
	if d.Class != "DC" || d.Span != comment || d.SourceSHA256 != shaOf(docSource) || d.ContextDigest != rep.Context.Digest {
		t.Errorf("diagnostic = %+v", d)
	}
	if d.Backend == nil || d.Backend.ID != "fake-dc" || d.Backend.Digest != "feed" || d.Score == nil || *d.Score != 0.8 || d.Message == "" {
		t.Errorf("diagnostic identity = backend %+v score %v message %q", d.Backend, d.Score, d.Message)
	}
	if c := coverageOf(t, f, "DC"); c.Status != diag.StatusChecked {
		t.Errorf("DC coverage = %s (%q), want checked", c.Status, c.Reason)
	}
	for _, class := range []string{"P1", "P2", "P3", "TF", "RP"} {
		if c := advisoryOf(t, f, class); c.Status != diag.StatusUnsupported || !strings.Contains(c.Reason, "fake-dc") {
			t.Errorf("%s coverage = %s (%q), want unsupported: the backend does not serve it", class, c.Status, c.Reason)
		}
	}
}

func TestAdviceFalseIsACheckedNegative(t *testing.T) {
	comment := spanOf(t, docSource, "// Café returns the count, obviously.", 0)
	rep := run(t, docSource, answer(func(f advisory.FileRef) []advisory.Prediction {
		return []advisory.Prediction{advised(f, comment, false, 0.1)}
	}))

	f := fileReport(t, rep, "demo.go")
	if got := found(f, diag.OriginAdvisory); got != "" {
		t.Errorf("advisory findings = %s, want none", got)
	}
	if c := coverageOf(t, f, "DC"); c.Status != diag.StatusChecked {
		t.Errorf("DC coverage = %s, want checked", c.Status)
	}
}

func TestRecordedAdviceMatchesBySourceHashAlone(t *testing.T) {
	comment := spanOf(t, docSource, "// Café returns the count, obviously.", 0)
	recording := advisory.Response{
		Schema:  advisory.Schema,
		Backend: advisory.Backend{ID: "recorded-fixture", Digest: "0", Detail: "authored"},
		Classes: []string{"DC"},
		Files:   []advisory.FileRecord{{SHA256: shaOf(docSource), Status: advisory.StatusChecked}},
		Predictions: []advisory.Prediction{{
			Class: "DC", SHA256: shaOf(docSource), Span: comment,
			Coverage: advisory.StatusChecked, Advice: ptr(true), Score: ptr(0.9),
		}},
	}
	body, err := json.Marshal(recording)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "recorded.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	rep := run(t, docSource, advisory.Recorded{Path: path})
	if rep.Advisory.Provider != "recorded" || rep.Advisory.Status != "ok" {
		t.Errorf("advisory = %+v", rep.Advisory)
	}
	if got, want := found(fileReport(t, rep, "demo.go"), diag.OriginAdvisory), expect(t, docSource, [2]string{"DC", "// Café returns the count, obviously."}); got != want {
		t.Errorf("advisory findings = %s, want %s", got, want)
	}

	// The same recording says nothing about any other revision of the file.
	edited := strings.Replace(docSource, "obviously", "plainly", 1)
	f := fileReport(t, run(t, edited, advisory.Recorded{Path: path}), "demo.go")
	if got := found(f, diag.OriginAdvisory); got != "" {
		t.Errorf("advisory findings on another revision = %s, want none", got)
	}
	if c := coverageOf(t, f, "DC"); c.Status != diag.StatusUnsupported || !strings.Contains(c.Reason, "no record") {
		t.Errorf("DC coverage = %s (%q), want unsupported: no record for this source", c.Status, c.Reason)
	}
}

func TestProviderTroubleIsAdvisoryErrorNeverANegative(t *testing.T) {
	comment := spanOf(t, docSource, "// Café returns the count, obviously.", 0)
	insideRune := diag.Span{Start: strings.Index(docSource, "é") + 1, End: comment.End}

	for name, provider := range map[string]advisory.Provider{
		"the backend fails": providerFunc(func(context.Context, advisory.Request) (advisory.Response, error) {
			return advisory.Response{}, errors.New("exit status 1: model artifact is missing")
		}),
		"a span runs past the source": answer(func(f advisory.FileRef) []advisory.Prediction {
			return []advisory.Prediction{advised(f, diag.Span{Start: comment.Start, End: len(docSource) + 1}, true, 0.8)}
		}),
		"a span splits a rune": answer(func(f advisory.FileRef) []advisory.Prediction {
			return []advisory.Prediction{advised(f, insideRune, true, 0.8)}
		}),
		"the response names a stale revision": answer(func(f advisory.FileRef) []advisory.Prediction {
			p := advised(f, comment, true, 0.8)
			p.SHA256 = shaOf("some other revision")
			return []advisory.Prediction{p}
		}),
	} {
		t.Run(name, func(t *testing.T) {
			rep := run(t, docSource, provider)
			if rep.Advisory.Status != diag.StatusError || rep.Advisory.Reason == "" {
				t.Errorf("advisory = %+v, want error with a reason", rep.Advisory)
			}
			f := fileReport(t, rep, "demo.go")
			if got := found(f, diag.OriginAdvisory); got != "" {
				t.Errorf("advisory findings = %s: nothing from an untrusted response may be reported", got)
			}
			for _, class := range diag.AdvisoryClasses {
				if c := advisoryOf(t, f, class); c.Status != diag.StatusError || c.Reason == "" {
					t.Errorf("%s coverage = %s (%q), want error with a reason", class, c.Status, c.Reason)
				}
			}
		})
	}
}

func TestAnUnsupportedCandidateMakesItsClassUnsupportedButKeepsItsFindings(t *testing.T) {
	comment := spanOf(t, docSource, "// Café returns the count, obviously.", 0)
	tooLong := spanOf(t, docSource, "func Café() int", 0)
	rep := run(t, docSource, answer(func(f advisory.FileRef) []advisory.Prediction {
		return []advisory.Prediction{
			advised(f, comment, true, 0.8),
			{Path: f.Path, Class: "DC", SHA256: f.SHA256, Span: tooLong, Coverage: advisory.StatusUnsupported, Reason: ptr("candidate exceeds the window")},
		}
	}))

	f := fileReport(t, rep, "demo.go")
	if got, want := found(f, diag.OriginAdvisory), expect(t, docSource, [2]string{"DC", "// Café returns the count, obviously."}); got != want {
		t.Errorf("advisory findings = %s, want %s kept", got, want)
	}
	c := coverageOf(t, f, "DC")
	if c.Status != diag.StatusUnsupported {
		t.Errorf("DC coverage = %s, want unsupported", c.Status)
	}
	if len(c.Items) != 1 || c.Items[0].Span == nil || *c.Items[0].Span != tooLong || c.Items[0].Reason != "candidate exceeds the window" {
		t.Errorf("DC coverage items = %+v, want the unsupported candidate", c.Items)
	}
}

func TestAFileTheBackendCouldNotReadIsClassError(t *testing.T) {
	rep := run(t, docSource, providerFunc(func(_ context.Context, req advisory.Request) (advisory.Response, error) {
		resp := advisory.Response{Schema: advisory.Schema, Backend: advisory.Backend{ID: "fake-dc"}, Classes: []string{"DC"}}
		for _, f := range req.Files {
			resp.Files = append(resp.Files, advisory.FileRecord{
				Path: f.Path, SHA256: f.SHA256, Status: advisory.StatusError, Reason: ptr("source changed since the request"),
			})
		}
		return resp, nil
	}))

	if rep.Advisory.Status != "ok" {
		t.Errorf("advisory = %+v: the exchange itself succeeded", rep.Advisory)
	}
	if c := coverageOf(t, fileReport(t, rep, "demo.go"), "DC"); c.Status != diag.StatusError || c.Reason != "source changed since the request" {
		t.Errorf("DC coverage = %s (%q), want the backend's error", c.Status, c.Reason)
	}
}

func TestExactOutputIsIdenticalUnderEveryProvider(t *testing.T) {
	comment := spanOf(t, docSource, "// Café returns the count, obviously.", 0)
	want := exactView(fileReport(t, run(t, docSource, nil), "demo.go"))
	if !strings.Contains(want, "P3@") {
		t.Fatalf("the fixture should carry an exact finding, got %q", want)
	}

	for name, provider := range map[string]advisory.Provider{
		"advising": answer(func(f advisory.FileRef) []advisory.Prediction {
			return []advisory.Prediction{advised(f, comment, true, 0.8)}
		}),
		"failing": providerFunc(func(context.Context, advisory.Request) (advisory.Response, error) {
			return advisory.Response{}, errors.New("boom")
		}),
		"malformed": answer(func(f advisory.FileRef) []advisory.Prediction {
			return []advisory.Prediction{advised(f, diag.Span{Start: 5, End: 1 << 20}, true, 0.8)}
		}),
	} {
		if got := exactView(fileReport(t, run(t, docSource, provider), "demo.go")); got != want {
			t.Errorf("%s provider: exact output = %q\nwant %q", name, got, want)
		}
	}
}

func TestOnlyTargetsAreSentAndTheExactReportIsNotMutated(t *testing.T) {
	var asked []string
	cfg := Config{Build: testBuild, Provider: providerFunc(func(_ context.Context, req advisory.Request) (advisory.Response, error) {
		for _, f := range req.Files {
			asked = append(asked, filepath.Base(f.Path))
			if !filepath.IsAbs(f.Path) {
				t.Errorf("request path %q is not absolute", f.Path)
			}
		}
		return answer(func(advisory.FileRef) []advisory.Prediction { return nil })(context.Background(), req)
	})}
	dir := writePkg(t, map[string]string{
		"demo.go":         docSource,
		"zz_generated.go": "// Code generated by authored-fixture. DO NOT EDIT.\n\npackage demo\n",
	})
	snap, exact, err := cfg.Exact(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := coverage(fileReport(t, exact, "demo.go"))

	cfg.Advise(context.Background(), []*pkgsnap.Snapshot{snap}, []diag.PackageReport{exact})

	if strings.Join(asked, ",") != "demo.go" {
		t.Errorf("asked about %v, want only the target", asked)
	}
	if after := coverage(fileReport(t, exact, "demo.go")); after != before {
		t.Errorf("Advise mutated the exact report it was given:\n%s\n%s", before, after)
	}
}

// sixSource carries exact P1, P2 and P3 findings at the sites that the learned classes also judge. sixTest is an
// ordinary test file with one TF candidate.
const (
	sixSource = "package demo\n\nimport \"errors\"\n\n// é😀 moves byte offsets.\nvar ErrBad = errors.New(\"Bad.\")\n\n" +
		"func Pick(ok bool) int {\n\tif ok {\n\t\treturn 1\n\t} else {\n\t\treturn 2\n\t}\n}\n\nfunc set(p_value int) int { return p_value }\n"
	sixTest = "package demo\n\nimport \"testing\"\n\nfunc TestInside(t *testing.T) {\n\tt.Error(\"inside\")\n}\n"
)

var sixBuild = pkgsnap.Build{GOOS: "linux", GOARCH: "amd64", TestTargets: true}

// sixAnswer is a six-class backend. Package files check the five package classes under the given context digest. Test
// files check TF only.
func sixAnswer(t *testing.T, digest string, withheld map[string]map[string]int) providerFunc {
	return func(_ context.Context, req advisory.Request) (advisory.Response, error) {
		resp := advisory.Response{
			Schema:  advisory.Schema,
			Backend: advisory.Backend{ID: "six-B", Digest: "feed", Detail: "arm B"},
			Classes: []string{"P1", "P2", "P3", "TF", "DC", "RP"},
		}
		for _, f := range req.Files {
			rec := advisory.FileRecord{Path: f.Path, SHA256: f.SHA256, Status: advisory.StatusChecked}
			predict := func(class, src, needle string, advice bool) {
				score := -0.25
				resp.Predictions = append(resp.Predictions, advisory.Prediction{
					Path: f.Path, Class: class, SHA256: f.SHA256, Span: spanOf(t, src, needle, 0),
					Coverage: advisory.StatusChecked, Advice: &advice, Score: &score,
				})
			}
			if strings.HasSuffix(f.Path, "_test.go") {
				rec.Classes = []string{"TF"}
				predict("TF", sixTest, `t.Error("inside")`, false)
			} else {
				rec.Classes, rec.Context, rec.Withheld = []string{"P1", "P2", "P3", "DC", "RP"}, digest, withheld
				predict("P1", sixSource, "else", true)
				predict("P2", sixSource, `"Bad."`, true)
				predict("P3", sixSource, "p_value", true)
			}
			resp.Files = append(resp.Files, rec)
		}
		return resp, nil
	}
}

// runSix analyses the six-class package with test targets under a provider built from the context digest of the
// snapshot itself.
func runSix(t *testing.T, provider func(digest string) advisory.Provider) diag.PackageReport {
	t.Helper()
	cfg := Config{Build: sixBuild}
	snap, rep, err := cfg.Exact(writePkg(t, map[string]string{"demo.go": sixSource, "demo_test.go": sixTest}), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Provider = provider(snap.Context.Digest())
	return cfg.Advise(context.Background(), []*pkgsnap.Snapshot{snap}, []diag.PackageReport{rep})[0]
}

func TestATestTargetIsJudgedOnlyByWhatAppliesToTestFiles(t *testing.T) {
	_, rep, err := Config{Build: sixBuild}.Exact(writePkg(t, map[string]string{"demo.go": sixSource, "demo_test.go": sixTest}), nil)
	if err != nil {
		t.Fatal(err)
	}
	f := fileReport(t, rep, "demo_test.go")
	if f.Role != diag.RoleTestTarget || len(f.Diagnostics) != 0 {
		t.Fatalf("demo_test.go = role %s, %d diagnostics; want a test target with no exact finding", f.Role, len(f.Diagnostics))
	}
	for _, class := range diag.ExactClasses {
		if c := coverageOf(t, f, class); c.Origin != diag.OriginExact || c.Status != diag.StatusUnsupported || !strings.Contains(c.Reason, "test files") {
			t.Errorf("exact %s on a test file = %s (%q), want unsupported: outside the retained evidence", class, c.Status, c.Reason)
		}
	}
	if c := advisoryOf(t, f, "TF"); c.Status != diag.StatusDisabled {
		t.Errorf("TF = %s, want disabled until advice runs", c.Status)
	}
}

func TestLearnedFindingsCoexistWithExactOnesAndTestFilesGetTFOnly(t *testing.T) {
	rep := runSix(t, func(digest string) advisory.Provider { return sixAnswer(t, digest, nil) })
	if rep.Advisory.Status != diag.AdvisoryOK {
		t.Fatalf("advisory = %+v", rep.Advisory)
	}

	f := fileReport(t, rep, "demo.go")
	same := expect(t, sixSource, [2]string{"P1", "else"}, [2]string{"P2", `"Bad."`}, [2]string{"P3", "p_value"})
	if got := found(f, diag.OriginExact); got != same {
		t.Errorf("exact findings = %s, want %s untouched by advice", got, same)
	}
	if got := found(f, diag.OriginAdvisory); got != same {
		t.Errorf("learned findings = %s, want %s beside the exact ones", got, same)
	}
	for _, d := range f.Diagnostics {
		if d.Origin == diag.OriginAdvisory && (d.Message == "" || d.Backend == nil || d.Backend.ID != "six-B" || d.Score == nil || *d.Score != -0.25) {
			t.Errorf("learned %s = %+v, want a named finding with the backend's raw score", d.Class, d)
		}
	}
	for _, class := range []string{"P1", "P2", "P3"} {
		if c := advisoryOf(t, f, class); c.Status != diag.StatusChecked || c.Candidates == nil || *c.Candidates != 1 {
			t.Errorf("learned %s = %s (%q) with %v candidates, want checked with one", class, c.Status, c.Reason, c.Candidates)
		}
		if c := coverageOf(t, f, class); c.Origin != diag.OriginExact || c.Status != diag.StatusChecked {
			t.Errorf("exact %s = %s %s, want checked exact coverage", class, c.Origin, c.Status)
		}
	}
	if c := advisoryOf(t, f, "TF"); c.Status != diag.StatusUnsupported || !strings.Contains(c.Reason, "did not check") || c.Candidates != nil {
		t.Errorf("TF on a package file = %s (%q), want unsupported without a count", c.Status, c.Reason)
	}

	test := fileReport(t, rep, "demo_test.go")
	if got := found(test, diag.OriginAdvisory); got != "" {
		t.Errorf("TF advice false must yield no finding, got %s", got)
	}
	if c := advisoryOf(t, test, "TF"); c.Status != diag.StatusChecked || c.Candidates == nil || *c.Candidates != 1 {
		t.Errorf("TF on the test file = %s (%q) with %v candidates, want checked with one", c.Status, c.Reason, c.Candidates)
	}
	for _, class := range []string{"P1", "P2", "P3", "DC", "RP"} {
		if c := advisoryOf(t, test, class); c.Status != diag.StatusUnsupported {
			t.Errorf("learned %s on a test file = %s, want unsupported", class, c.Status)
		}
	}
}

func TestLearnedP2IsMergedOnlyUnderTheContextItWasCertifiedFor(t *testing.T) {
	for name, tc := range map[string]struct {
		context func(digest string) string
		status  string
		reason  string
	}{
		"matching":  {func(d string) string { return d }, diag.StatusChecked, ""},
		"differing": {func(string) string { return strings.Repeat("0", 64) }, diag.StatusError, "0000000000"},
		"absent":    {func(string) string { return "" }, diag.StatusUnsupported, "no package context"},
	} {
		t.Run(name, func(t *testing.T) {
			var digest string
			rep := runSix(t, func(d string) advisory.Provider {
				digest = d
				return sixAnswer(t, tc.context(d), nil)
			})
			f := fileReport(t, rep, "demo.go")
			c := advisoryOf(t, f, "P2")
			if c.Status != tc.status || !strings.Contains(c.Reason, tc.reason) {
				t.Errorf("learned P2 = %s (%q), want %s naming %q", c.Status, c.Reason, tc.status, tc.reason)
			}
			if tc.status == diag.StatusError && !strings.Contains(c.Reason, digest[:12]) {
				t.Errorf("the error %q must name the snapshot digest %s too", c.Reason, digest[:12])
			}
			learned := found(f, diag.OriginAdvisory)
			if tc.status != diag.StatusChecked && (strings.Contains(learned, "P2@") || c.Candidates != nil) {
				t.Errorf("learned findings %s and count %v: nothing of P2 may be merged", learned, c.Candidates)
			}
			if !strings.Contains(learned, "P1@") || advisoryOf(t, f, "P1").Status != diag.StatusChecked {
				t.Errorf("learned P1 must merge whatever the P2 context: %s", learned)
			}
		})
	}
}

func TestWithheldCountsAreUnjudgedCandidatesWithoutASpan(t *testing.T) {
	withheld := map[string]map[string]int{"DC": {"selector withheld: no_prose": 2}}
	rep := runSix(t, func(d string) advisory.Provider { return sixAnswer(t, d, withheld) })

	c := advisoryOf(t, fileReport(t, rep, "demo.go"), "DC")
	if c.Status != diag.StatusUnsupported || c.Candidates == nil || *c.Candidates != 2 || len(c.Items) != 2 {
		t.Fatalf("DC = %s (%q), %v candidates, items %+v; want unsupported with two unjudged candidates", c.Status, c.Reason, c.Candidates, c.Items)
	}
	for _, item := range c.Items {
		if item.Span != nil || item.Status != diag.StatusUnsupported || !strings.Contains(item.Reason, "no_prose") {
			t.Errorf("item = %+v, want a span-less unsupported item naming its reason", item)
		}
	}
	if rp := advisoryOf(t, fileReport(t, rep, "demo.go"), "RP"); rp.Status != diag.StatusChecked || rp.Candidates == nil || *rp.Candidates != 0 {
		t.Errorf("RP = %s with %v candidates, want checked with zero", rp.Status, rp.Candidates)
	}
}

// A presenter must be able to say how many candidates the backend reported for a file and class, whatever it made of
// them. The count is the number of validated, deduplicated predictions of the class, including a checked negative and
// an unjudged candidate. It is never the number of findings.
func TestReportedCandidatesAreCountedFromValidatedPredictionsNotFindings(t *testing.T) {
	comment := spanOf(t, docSource, "// Café returns the count, obviously.", 0)
	tooLong := spanOf(t, docSource, "func Café() int", 0)
	rep := run(t, docSource, answer(func(f advisory.FileRef) []advisory.Prediction {
		return []advisory.Prediction{
			advised(f, comment, false, 0.1),
			advised(f, comment, false, 0.1), // the contract allows an exact duplicate, and it counts once
			{Path: f.Path, Class: "DC", SHA256: f.SHA256, Span: tooLong, Coverage: advisory.StatusUnsupported, Reason: ptr("candidate exceeds the window")},
		}
	}))

	f := fileReport(t, rep, "demo.go")
	if c := coverageOf(t, f, "DC"); c.Candidates == nil || *c.Candidates != 2 {
		t.Errorf("DC candidates = %v, want 2: a negative and an unjudged candidate were both reported", c.Candidates)
	}
	if got := found(f, diag.OriginAdvisory); got != "" {
		t.Errorf("advisory findings = %s, want none: the count must not come from findings", got)
	}
	if c := advisoryOf(t, f, "RP"); c.Candidates != nil {
		t.Errorf("RP candidates = %d, want no count for an unserved class", *c.Candidates)
	}
	if c := coverageOf(t, f, "P1"); c.Origin != diag.OriginExact || c.Candidates != nil {
		t.Errorf("exact P1 candidates = %v, want no count for an exact class", c.Candidates)
	}
}

func TestACheckedFileWithoutPredictionsCountsZeroAndAnAbsentCountStaysAbsent(t *testing.T) {
	rep := run(t, docSource, answer(func(advisory.FileRef) []advisory.Prediction { return nil }))
	if c := coverageOf(t, fileReport(t, rep, "demo.go"), "DC"); c.Candidates == nil || *c.Candidates != 0 {
		t.Errorf("DC candidates = %v, want 0 for a checked file the backend reported no candidate for", c.Candidates)
	}

	for name, provider := range map[string]advisory.Provider{
		"disabled": nil,
		"failing": providerFunc(func(context.Context, advisory.Request) (advisory.Response, error) {
			return advisory.Response{}, errors.New("boom")
		}),
		"the file record is an error": providerFunc(func(_ context.Context, req advisory.Request) (advisory.Response, error) {
			resp := advisory.Response{Schema: advisory.Schema, Backend: advisory.Backend{ID: "fake-dc"}, Classes: []string{"DC"}}
			for _, f := range req.Files {
				resp.Files = append(resp.Files, advisory.FileRecord{Path: f.Path, SHA256: f.SHA256, Status: advisory.StatusError, Reason: ptr("source changed")})
			}
			return resp, nil
		}),
		"no record for the source": providerFunc(func(context.Context, advisory.Request) (advisory.Response, error) {
			return advisory.Response{Schema: advisory.Schema, Backend: advisory.Backend{ID: "fake-dc"}, Classes: []string{"DC"}}, nil
		}),
	} {
		if c := coverageOf(t, fileReport(t, run(t, docSource, provider), "demo.go"), "DC"); c.Candidates != nil {
			t.Errorf("%s: DC candidates = %d, want no count: an absent count must not read as zero", name, *c.Candidates)
		}
	}
}
