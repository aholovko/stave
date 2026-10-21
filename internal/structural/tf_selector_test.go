package structural

import (
	"reflect"
	"strings"
	"testing"
)

const tfNested = "package p\n\nimport \"testing\"\n\nfunc TestR(t *testing.T) {\n\tt.Run(\"a\", func(u *testing.T) {\n\t\tu.Run(\"b\", func(v *testing.T) {\n\t\t\tv.Error(\"x\")\n\t\t})\n\t})\n}\n"

func tfStream(t *testing.T, src string) Stream {
	t.Helper()
	streams, _, err := BuildTFLedger("x", "x_test.go", shaOf(src), []byte(src))
	if err != nil || len(streams) != 1 {
		t.Fatalf("%d streams, %v", len(streams), err)
	}
	return streams[0]
}

func markAt(t *testing.T, events []Event, kind string, n int) int {
	t.Helper()
	for i, e := range events {
		if e.E == "MARK" && e.K == kind {
			if n == 0 {
				return i
			}
			n--
		}
	}
	t.Fatalf("no MARK %s", kind)
	return -1
}

func edited(events []Event, edit func([]Event) []Event) []Event {
	return edit(append([]Event(nil), events...))
}

func TestTFSelectorMarksAreNonOwningAndRunPairsOptional(t *testing.T) {
	s := tfStream(t, tfNested)
	full := Select(s.Header, s.Events)
	if !full.OK {
		t.Fatalf("%s at %d", full.Reason, *full.EventIndex)
	}
	var bare []Event
	for _, e := range s.Events {
		if e.E != "MARK" || e.K == "CandArgs" || e.K == "CandBind" {
			bare = append(bare, e)
		}
	}
	if len(bare) != len(s.Events)-4 {
		t.Fatalf("expected two Run pairs, removed %d events", len(s.Events)-len(bare))
	}
	omitted := Select(s.Header, bare)
	if !omitted.OK || !reflect.DeepEqual(omitted.Facts, full.Facts) || !reflect.DeepEqual(omitted.Runs, full.Runs) {
		t.Fatalf("omitting Run pairs changed ownership: %+v", omitted)
	}
	if got := texts(t, tfNested, full, "RUN_PREFIX"); !reflect.DeepEqual(got, []string{"\"a\"", "\"b\""}) {
		t.Fatalf("full prefix %q", got)
	}
	if omitted.Roles["RUN_PREFIX"].Why != "structural_empty" {
		t.Fatalf("omitted prefix %+v", omitted.Roles["RUN_PREFIX"])
	}
	// The inner pair alone stops the chain at the outer literal without a pair.
	inner := edited(s.Events, func(ev []Event) []Event {
		i := markAt(t, ev, "RunName", 0)
		return append(ev[:i], ev[i+2:]...)
	})
	if r := Select(s.Header, inner); !r.OK || !reflect.DeepEqual(texts(t, tfNested, r, "RUN_PREFIX"), []string{"\"b\""}) {
		t.Fatalf("inner-only prefix %+v", r)
	}
}

func TestTFSelectorRejectsMalformedMarkerSequences(t *testing.T) {
	s := tfStream(t, tfNested)
	cases := map[string]struct {
		edit func([]Event) []Event
		want string
	}{
		"unknown kind": {func(ev []Event) []Event {
			ev[markAt(t, ev, "CandBind", 0)].K = "Bogus"
			return ev
		}, "unknown marker"},
		"missing CandArgs": {func(ev []Event) []Event {
			i := markAt(t, ev, "CandArgs", 0)
			return append(ev[:i], ev[i+1:]...)
		}, "expected marker CandArgs"},
		"duplicate CandArgs": {func(ev []Event) []Event {
			i := markAt(t, ev, "CandArgs", 0)
			return append(ev[:i+1], ev[i:]...)
		}, "expected marker CandBind"},
		"CandArgs not strictly inside": {func(ev []Event) []Event {
			i := markAt(t, ev, "CandArgs", 0)
			ev[i].S = ev[i-1].S
			return ev
		}, "strictly inside"},
		"CandBind not an open frame": {func(ev []Event) []Event {
			ev[markAt(t, ev, "CandBind", 0)].S++
			return ev
		}, "open root or FuncLit"},
		"RunName without RunBind": {func(ev []Event) []Event {
			i := markAt(t, ev, "RunBind", 1)
			return append(ev[:i], ev[i+1:]...)
		}, "expected marker RunBind"},
		"pair before a non-literal OPEN": {func(ev []Event) []Event {
			i := markAt(t, ev, "RunName", 0)
			pair := []Event{ev[i], ev[i+1]}
			ev = append(ev[:i], ev[i+2:]...)
			for j, e := range ev {
				if e.E == "OPEN" && e.K == "Expr" {
					return append(ev[:j], append(pair, ev[j:]...)...)
				}
			}
			return ev
		}, "precede a FuncLit OPEN"},
		"RunName after the literal start": {func(ev []Event) []Event {
			i := markAt(t, ev, "RunName", 0)
			ev[i].S, ev[i].T = ev[i+2].S, ev[i+2].S+1
			return ev
		}, "end by the literal start"},
		"RunBind not an open frame": {func(ev []Event) []Event {
			ev[markAt(t, ev, "RunBind", 1)].T--
			return ev
		}, "open root or FuncLit"},
		"CAND not after its Expr OPEN": {func(ev []Event) []Event {
			for i, e := range ev {
				if e.E == "CAND" {
					ev[i-1], ev[i] = ev[i], ev[i-1]
					return ev
				}
			}
			return ev
		}, "immediately follow the OPEN of its Expr"},
		"marker outside any frame": {func(ev []Event) []Event {
			return append(ev, Event{E: "MARK", K: "RunName", S: 0, T: 1})
		}, "outside any frame"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := Select(s.Header, edited(s.Events, tc.edit))
			if r.OK || !strings.Contains(r.Reason, tc.want) || r.Facts != nil || r.Roles != nil {
				t.Fatalf("got ok=%v reason %q, want %q", r.OK, r.Reason, tc.want)
			}
		})
	}
}

func TestTFSelectorMarkersOnlyInTFStreams(t *testing.T) {
	src := "package p\n\n// F doc.\nfunc F(a bool) {\n\tif a {\n\t} else {\n\t}\n}\n"
	streams, err := Build("P1", "x", []byte(src))
	if err != nil || len(streams) != 1 {
		t.Fatalf("%d %v", len(streams), err)
	}
	ev := append([]Event{}, streams[0].Events[:1]...)
	ev = append(ev, Event{E: "MARK", K: "RunName", S: 0, T: 1})
	ev = append(ev, streams[0].Events[1:]...)
	if r := Select(streams[0].Header, ev); r.OK || !strings.Contains(r.Reason, "only in TF streams") {
		t.Fatalf("P1 with MARK: %+v", r)
	}
	if r := Select(streams[0].Header, streams[0].Events); !r.OK {
		t.Fatalf("P1 unchanged: %s", r.Reason)
	}
}
