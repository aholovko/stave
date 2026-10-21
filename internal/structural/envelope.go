package structural

import (
	"errors"
	"fmt"
)

// selectEnvelope checks a stream against the P2/P3 four-event grammar:
//
//	OPEN  k=CallEnv|DeclEnv  s=a t=b   0 <= a < b <= len; sole root
//	LEAF  k=Msg|DeclName     s=c t=d   a <= c < d <= b; P2 also d = b
//	CAND  id=<header cand>   s=c t=d
//	CLOSE k=<same kind>
//
// It derives one run, one envelope fact and the CAND and CALL|DECL roles. Any other event, field, kind or span fails
// the candidate at the first violating event. A short stream fails at its end.
func selectEnvelope(h Header, events []Event) Result {
	res := Result{Task: h.Task, Cand: h.Cand, Source: Source{Bytes: h.Len, SHA256: h.SHA256}}
	fail := func(i int, err error) Result {
		res.Reason, res.EventIndex = err.Error(), &i
		return res
	}
	if h.Cand == "" {
		return fail(-1, errors.New("header has no candidate id"))
	}
	kinds := envelopeKinds[h.Task]
	var env, leaf [2]int
	for i, e := range events {
		if i == 4 {
			return fail(i, errors.New("event after the envelope CLOSE"))
		}
		if err := envelopeEvent(i, e, h, kinds, &env, &leaf); err != nil {
			return fail(i, err)
		}
	}
	if len(events) < 4 {
		return fail(len(events), errors.New("stream ends before the envelope CLOSE"))
	}
	whole := map[string]string{"P2": "CALL", "P3": "DECL"}[h.Task]
	res.OK = true
	res.Runs = [][2]int{env}
	res.Facts = []Fact{{S: env[0], T: env[1], Kind: "NoStmt", Parent: "Outside", Ordinal: -1, Depth: -1}}
	res.Roles = map[string]Role{
		"CAND": {IV: [][2]int{leaf}, Why: "present"},
		whole:  {IV: [][2]int{env}, Why: "present"},
	}
	return res
}

func envelopeEvent(i int, e Event, h Header, kinds [2]string, env, leaf *[2]int) error {
	want := [4]string{"OPEN", "LEAF", "CAND", "CLOSE"}[i]
	switch {
	case e.E == "BAD":
		return fmt.Errorf("bad event: %s", e.Reason)
	case e.E != want:
		return fmt.Errorf("event %s where the envelope grammar requires %s", e.E, want)
	case e.N != "" || e.Single || e.Reason != "":
		return fmt.Errorf("%s carries a slot or reason field", e.E)
	case e.E != "CAND" && e.ID != "":
		return fmt.Errorf("%s carries a candidate id", e.E)
	}
	switch i {
	case 0:
		if e.K != kinds[0] {
			return fmt.Errorf("root kind %s, want %s", e.K, kinds[0])
		}
		if e.S < 0 || e.S >= e.T || e.T > h.Len {
			return fmt.Errorf("envelope [%d, %d) is empty or outside the %d-byte source", e.S, e.T, h.Len)
		}
		*env = [2]int{e.S, e.T}
	case 1:
		if e.K != kinds[1] {
			return fmt.Errorf("leaf kind %s, want %s", e.K, kinds[1])
		}
		if e.S < env[0] || e.S >= e.T || e.T > env[1] {
			return fmt.Errorf("leaf [%d, %d) is empty or outside the envelope %v", e.S, e.T, *env)
		}
		if h.Task == "P2" && e.T != env[1] {
			return fmt.Errorf("P2 leaf ends at %d, not at the envelope end %d", e.T, env[1])
		}
		*leaf = [2]int{e.S, e.T}
	case 2:
		if e.K != "" {
			return errors.New("CAND carries a kind")
		}
		if e.ID != h.Cand {
			return fmt.Errorf("candidate id %q differs from header %q", e.ID, h.Cand)
		}
		if [2]int{e.S, e.T} != *leaf {
			return fmt.Errorf("CAND [%d, %d) differs from the leaf %v", e.S, e.T, *leaf)
		}
	case 3:
		if e.K != kinds[0] {
			return fmt.Errorf("close kind %s does not match open %s", e.K, kinds[0])
		}
		if e.S != 0 || e.T != 0 {
			return errors.New("CLOSE carries a span")
		}
	}
	return nil
}
