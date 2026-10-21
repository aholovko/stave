package diag

import (
	"strings"
	"testing"
)

func TestNotJudgedMergesNeighboursThatShareAnOriginAndAnExplanation(t *testing.T) {
	incomplete := []CoverageItem{{Status: StatusUnsupported, Reason: "broken.go: parse-error"}}
	coverage := []Coverage{
		{Class: "P1", Origin: OriginExact, Status: StatusChecked},
		{Class: "P2", Origin: OriginExact, Status: StatusUnsupported, Reason: "incomplete package context", Items: incomplete},
		{Class: "P3", Origin: OriginExact, Status: StatusChecked},
		{Class: "P1", Origin: OriginAdvisory, Status: StatusChecked},
		{Class: "P2", Origin: OriginAdvisory, Status: StatusUnsupported, Reason: "incomplete package context", Items: incomplete},
		{Class: "TF", Origin: OriginAdvisory, Status: StatusUnsupported, Reason: "not served"},
		{Class: "RP", Origin: OriginAdvisory, Status: StatusUnsupported, Reason: "not served"},
		{Class: "DC", Origin: OriginAdvisory, Status: StatusUnsupported, Reason: "1 candidate(s) could not be judged", Items: []CoverageItem{
			{Span: &Span{Start: 5, End: 9}, Status: StatusUnsupported, Reason: "too long"},
		}},
	}

	want := "P2 exact unsupported: incomplete package context [broken.go: parse-error]\n" +
		"P2 advisory unsupported: incomplete package context [broken.go: parse-error]\n" +
		"TF RP advisory unsupported: not served\n" +
		"DC advisory unsupported: 1 candidate(s) could not be judged [bytes [5,9): too long]"
	if got := strings.Join(NotJudged(coverage), "\n"); got != want {
		t.Errorf("NotJudged =\n%s\nwant\n%s", got, want)
	}
	if got := NotJudged(coverage[:1]); len(got) != 0 {
		t.Errorf("a fully checked file has nothing to state, got %v", got)
	}
}

// Exact and advisory findings share class IDs; presenters show their origin separately.
func TestClassesKeepTheirOrderAndCandidateLabels(t *testing.T) {
	if got := strings.Join(ExactClasses, " "); got != "P1 P2 P3" {
		t.Errorf("exact classes = %s, want P1 P2 P3", got)
	}
	if got := strings.Join(AdvisoryClasses, " "); got != "P1 P2 P3 TF DC RP" {
		t.Errorf("advisory classes = %s, want the six by stable ID", got)
	}
	n := 2
	learned := Coverage{Class: "P3", Origin: OriginAdvisory, Status: StatusChecked, Candidates: &n}
	if got := Candidates(learned, 1); got != "P3: 2 candidates reported, 1 advised" {
		t.Errorf("Candidates = %q", got)
	}
}

// An absent candidate count must not read as zero.
func TestCandidatesStatesReportedCountsAndNeverInventsOne(t *testing.T) {
	n := func(v int) *int { return &v }
	unjudged := []CoverageItem{{Span: &Span{Start: 5, End: 9}, Status: StatusUnsupported, Reason: "too long"}}
	failed := []CoverageItem{{Span: &Span{Start: 5, End: 9}, Status: StatusError, Reason: "the backend crashed on it"}}
	for name, tc := range map[string]struct {
		c       Coverage
		advised int
		want    string
	}{
		"no count": {Coverage{Class: "DC", Status: StatusUnsupported, Reason: "backend x does not serve this class"}, 0, ""},
		"exact":    {Coverage{Class: "P2", Origin: OriginExact, Status: StatusChecked}, 0, ""},
		"zero":     {Coverage{Class: "DC", Status: StatusChecked, Candidates: n(0)}, 0, "DC: 0 candidates reported"},
		"one":      {Coverage{Class: "DC", Status: StatusChecked, Candidates: n(1)}, 1, "DC: 1 candidate reported, 1 advised"},
		"plain":    {Coverage{Class: "RP", Status: StatusChecked, Candidates: n(2)}, 0, "RP: 2 candidates reported, 0 advised"},
		"partly":   {Coverage{Class: "DC", Status: StatusUnsupported, Candidates: n(3), Items: unjudged}, 1, "DC: 3 candidates reported, 1 advised, 1 not judged"},
		"error":    {Coverage{Class: "DC", Status: StatusError, Candidates: n(2), Items: failed}, 0, "DC: 2 candidates reported, 0 advised, 1 not judged (error)"},
	} {
		if got := Candidates(tc.c, tc.advised); got != tc.want {
			t.Errorf("%s: %q\nwant %q", name, got, tc.want)
		}
	}
}
