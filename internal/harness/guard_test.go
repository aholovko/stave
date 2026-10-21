package harness

import (
	"strings"
	"testing"

	"github.com/aholovko/stave/internal/diag"
	"github.com/aholovko/stave/internal/span"
)

// Use synthetic analyzer spans to test the adapter independently, including
// shortened raw-string spans from CR normalization.
func occ(class string, start, end int) span.Occurrence {
	return span.Occurrence{Class: class, Span: span.Span{Start: start, End: end}}
}

func TestARawP2LiteralEndsWhereTheOriginalBytesCloseIt(t *testing.T) {
	const src = "package demo\r\n\r\nvar Err = errors.New(`Bad\r\nthing.`)\r\n"
	want := spanOf(t, src, "`Bad\r\nthing.`", 0)
	// A scanner that strips the CR from the value of the literal reports an end one byte short of the closing
	// backtick.
	short := occ("P2", want.Start, want.End-1)

	findings, failed := adapt([]byte(src), []span.Occurrence{short})

	if len(failed) != 0 {
		t.Fatalf("failed = %v, want the short end repaired from the original bytes", failed)
	}
	if got := findings["P2"]; len(got) != 1 || got[0] != want {
		t.Errorf("P2 spans = %+v, want %+v", got, want)
	}
}

func TestASpanThatContradictsTheOriginalBytesFailsItsClassClosed(t *testing.T) {
	const src = "package demo\n\nvar count int = 1 // else\n\nvar Err = errors.New(\"Bad thing.\")\n\nvar snake_case, DemoThing = 1, 2\n"
	at := func(needle string) diag.Span { return spanOf(t, src, needle, 0) }
	shift := func(s diag.Span) span.Span { return span.Span{Start: s.Start + 1, End: s.End + 1} }

	for _, tc := range []struct {
		class, needle string
	}{
		{"P1", "else"},
		{"P2", `"Bad thing."`},
		{"P3", "snake_case"},
	} {
		t.Run(tc.class, func(t *testing.T) {
			good := at(tc.needle)
			findings, failed := adapt([]byte(src), []span.Occurrence{occ(tc.class, good.Start, good.End)})
			if len(failed) != 0 || len(findings[tc.class]) != 1 || findings[tc.class][0] != good {
				t.Fatalf("a span on the expected bytes must pass: findings %v, failed %v", findings, failed)
			}

			bad := shift(good)
			findings, failed = adapt([]byte(src), []span.Occurrence{
				occ(tc.class, good.Start, good.End),
				{Class: tc.class, Span: bad},
			})
			if len(findings[tc.class]) != 0 {
				t.Errorf("findings = %v: one untrustworthy span must withhold the whole class", findings[tc.class])
			}
			if !strings.Contains(failed[tc.class], tc.class) {
				t.Errorf("failed = %v, want a reason naming %s", failed, tc.class)
			}
		})
	}
}

func TestAFileTheAnalyzerCannotBindReportsErrorCoverageNotSilence(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"demo.go":  "package demo\n\nvar count int = 1\n",
		"other.go": "package demo\n\nvar other_name = 2\n",
	})
	snap, _, err := Config{Build: testBuild}.Exact(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The bytes no longer hash to the manifest row, so no instance exists.
	for i := range snap.Files {
		if snap.Files[i].Name == "demo.go" {
			snap.Files[i].Src = []byte("package demo\n\nvar count int = 2\n")
		}
	}

	rep := Report(snap)

	f := fileReport(t, rep, "demo.go")
	if len(f.Diagnostics) != 0 {
		t.Errorf("diagnostics = %+v, want none from an unbindable file", f.Diagnostics)
	}
	for _, class := range diag.ExactClasses {
		if c := coverageOf(t, f, class); c.Status != diag.StatusError || c.Reason == "" {
			t.Errorf("%s coverage = %s (%q), want error with a reason", class, c.Status, c.Reason)
		}
	}
	if got := found(fileReport(t, rep, "other.go"), diag.OriginExact); got == "" {
		t.Error("one unbindable file must not silence its siblings")
	}
}

func TestAnP2SpanMustBeOneWholeStringLiteral(t *testing.T) {
	const src = "package demo\n\nvar Err = errors.New(`never closed)\n"
	start := strings.Index(src, "`")

	_, failed := adapt([]byte(src), []span.Occurrence{occ("P2", start, start+5)})

	if failed["P2"] == "" {
		t.Error("an unterminated raw literal has no end in the original bytes and must fail closed")
	}
}
