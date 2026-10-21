package structural_test

import (
	"encoding/json"
	"testing"

	"github.com/aholovko/stave/internal/structural"
)

func envStream(task string) (structural.Header, []structural.Event) {
	frame, leaf := "CallEnv", "Msg"
	if task == "P3" {
		frame, leaf = "DeclEnv", "DeclName"
	}
	h := structural.Header{Task: task, Cand: task + ":fx:0", Len: 40, SHA256: "ab"}
	return h, []structural.Event{
		{E: "OPEN", K: frame, S: 10, T: 30},
		{E: "LEAF", K: leaf, S: 20, T: 30},
		{E: "CAND", ID: task + ":fx:0", S: 20, T: 30},
		{E: "CLOSE", K: frame},
	}
}

func TestEnvelopeSuccessJSON(t *testing.T) {
	for task, whole := range map[string]string{"P2": "CALL", "P3": "DECL"} {
		h, ev := envStream(task)
		b, err := json.Marshal(structural.Select(h, ev))
		if err != nil {
			t.Fatal(err)
		}
		roles := `"CAND":{"iv":[[20,30]],"why":"present"},"` + whole + `":{"iv":[[10,30]],"why":"present"}`
		if whole == "CALL" { // map keys serialize sorted
			roles = `"CALL":{"iv":[[10,30]],"why":"present"},"CAND":{"iv":[[20,30]],"why":"present"}`
		}
		want := `{"ok":true,"task":"` + task + `","cand":"` + task + `:fx:0","source":{"bytes":40,"sha256":"ab"},` +
			`"runs":[[10,30]],"roles":{` + roles + `},"facts":[[10,30,"NoStmt","Outside",null,null,null,null,0]]}`
		if string(b) != want {
			t.Errorf("%s:\n got %s\nwant %s", task, b, want)
		}
	}
	// P3 leaves may end before the envelope and start at it.
	h, ev := envStream("P3")
	ev[0].S, ev[1].S, ev[1].T, ev[2].S, ev[2].T = 20, 20, 25, 20, 25
	if r := structural.Select(h, ev); !r.OK {
		t.Errorf("P3 inner leaf: %s", r.Reason)
	}
}

func TestEnvelopeMalformedStreams(t *testing.T) {
	cases := []struct {
		name, task string
		mutate     func(h *structural.Header, ev *[]structural.Event)
		index      int
	}{
		{"empty cand header", "P2", func(h *structural.Header, _ *[]structural.Event) { h.Cand = "" }, -1},
		{"empty stream", "P2", func(_ *structural.Header, ev *[]structural.Event) { *ev = nil }, 0},
		{"short stream", "P3", func(_ *structural.Header, ev *[]structural.Event) { *ev = (*ev)[:3] }, 3},
		{"trailing event", "P2", func(_ *structural.Header, ev *[]structural.Event) {
			*ev = append(*ev, structural.Event{E: "CLOSE", K: "CallEnv"})
		}, 4},
		{"second root", "P2", func(_ *structural.Header, ev *[]structural.Event) {
			*ev = append(*ev, (*ev)[0], (*ev)[1], (*ev)[2], (*ev)[3])
		}, 4},
		{"first not OPEN", "P2", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[0].E = "LEAF" }, 0},
		{"wrong frame kind", "P2", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[0].K = "DeclEnv" }, 0},
		{"statement frame", "P3", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[0].K = "Expr" }, 0},
		{"empty envelope", "P2", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[0].S = 30 }, 0},
		{"negative start", "P2", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[0].S = -1 }, 0},
		{"beyond source", "P2", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[0].T = 41 }, 0},
		{"open slot field", "P2", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[0].N = "Then" }, 0},
		{"open single", "P2", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[0].Single = true }, 0},
		{"open id", "P2", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[0].ID = "x" }, 0},
		{"bad event", "P2", func(_ *structural.Header, ev *[]structural.Event) {
			(*ev)[1] = structural.Event{E: "BAD", Reason: "x"}
		}, 1},
		{"slot instead of leaf", "P2", func(_ *structural.Header, ev *[]structural.Event) {
			(*ev)[1] = structural.Event{E: "SLOT", N: "Then", S: 20, T: 30}
		}, 1},
		{"nested open", "P3", func(_ *structural.Header, ev *[]structural.Event) {
			(*ev)[1] = structural.Event{E: "OPEN", K: "DeclEnv", S: 20, T: 30}
		}, 1},
		{"wrong leaf kind", "P2", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[1].K = "DeclName" }, 1},
		{"empty leaf", "P3", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[1].T = 20 }, 1},
		{"leaf before envelope", "P3", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[1].S = 9 }, 1},
		{"leaf after envelope", "P3", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[1].T = 31 }, 1},
		{"P2 leaf short of envelope end", "P2", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[1].T = 29 }, 1},
		{"leaf id", "P2", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[1].ID = "x" }, 1},
		{"second leaf", "P2", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[2] = (*ev)[1] }, 2},
		{"cand id", "P2", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[2].ID = "P2:fx:1" }, 2},
		{"cand span", "P3", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[2].S = 21 }, 2},
		{"cand kind", "P3", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[2].K = "DeclName" }, 2},
		{"close kind", "P2", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[3].K = "DeclEnv" }, 3},
		{"close span", "P2", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[3].T = 30 }, 3},
		{"second cand", "P2", func(_ *structural.Header, ev *[]structural.Event) { (*ev)[3] = (*ev)[2] }, 3},
	}
	for _, c := range cases {
		h, ev := envStream(c.task)
		c.mutate(&h, &ev)
		r := structural.Select(h, ev)
		if r.OK || r.Reason == "" || r.EventIndex == nil || *r.EventIndex != c.index ||
			r.Runs != nil || r.Roles != nil || r.Facts != nil {
			idx := -99
			if r.EventIndex != nil {
				idx = *r.EventIndex
			}
			t.Errorf("%s: ok %v reason %q index %d, want failure at %d", c.name, r.OK, r.Reason, idx, c.index)
		}
	}
}

// A valid envelope must cover its run exactly and retain both roles, with CAND
// inside the run.
func TestEnvelopeInvariantsOnFixtures(t *testing.T) {
	for _, fx := range acceptedFixtures {
		streams, _, err := structural.BuildLedger(fx.task, "fx", []byte(fx.src), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range streams {
			r := structural.Select(s.Header, s.Events)
			if !r.OK {
				t.Fatalf("%s %s: %s", fx.name, s.Header.Cand, r.Reason)
			}
			run, cand := r.Runs[0], r.Roles["CAND"].IV[0]
			if len(r.Runs) != 1 || len(r.Facts) != 1 || r.Facts[0].S != run[0] || r.Facts[0].T != run[1] ||
				cand[0] < run[0] || cand[1] > run[1] || cand[0] >= cand[1] || len(r.Roles) != 2 {
				t.Errorf("%s %s: result %+v", fx.name, s.Header.Cand, r)
			}
		}
	}
}
