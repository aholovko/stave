package structural

import (
	"errors"
	"fmt"
	"slices"
)

// packetVariants are the run-kind sequences each DC/RP packet may take.
var packetVariants = map[string][][]string{
	"DC": {{"Doc", "Decl"}, {"Owner", "Doc", "Decl"}},
	"RP": {{"Decl"}, {"Doc", "Decl"}, {"Owner", "Decl"}, {"Owner", "Doc", "Decl"}},
}

// packetLeaf names, for each task, the designated run kind and the leaf kind that its CAND repeats.
var packetLeaf = map[string][2]string{"DC": {"Doc", "Comment"}, "RP": {"Decl", "Results"}}

// selectPacket validates the DC/RP packet grammar:
//
//	run    := OPEN CLOSE | OPEN LEAF CAND CLOSE
//	stream := run{1,3}
//
// Run kinds must match packetVariants. Nonempty runs must be separated by at least
// one byte. Exactly one designated run carries the leaf and matching CAND:
// DC uses the whole Doc run; RP uses the trailing result list in Decl.
//
// The selector derives roles and facts only after validating the packet, so a
// malformed stream cannot yield partial features.
func selectPacket(h Header, events []Event) Result {
	res := Result{Task: h.Task, Cand: h.Cand, Source: Source{Bytes: h.Len, SHA256: h.SHA256}}
	fail := func(i int, err error) Result {
		res.Reason, res.EventIndex = err.Error(), &i
		return res
	}
	if h.Cand == "" {
		return fail(-1, errors.New("header has no candidate id"))
	}
	designated := packetLeaf[h.Task]
	var kinds []string
	var runs [][2]int
	var leaf [2]int
	state := "between" // between, run, leaf, cand
	for i, e := range events {
		if err := packetEvent(h, e, state, designated, kinds, runs, &leaf); err != nil {
			return fail(i, err)
		}
		switch state {
		case "between":
			kinds, runs = append(kinds, e.K), append(runs, [2]int{e.S, e.T})
			state = "run"
		case "run":
			state = map[string]string{"LEAF": "leaf", "CLOSE": "between"}[e.E]
		case "leaf":
			state = "cand"
		case "cand":
			state = "between"
		}
	}
	if state != "between" {
		return fail(len(events), errors.New("stream ends inside a run"))
	}
	if !slices.ContainsFunc(packetVariants[h.Task], func(v []string) bool { return slices.Equal(v, kinds) }) {
		return fail(len(events), fmt.Errorf("run kinds %v are no %s variant", kinds, h.Task))
	}
	res.OK, res.Runs = true, runs
	decl, doc := [][2]int{}, [][2]int{}
	for k, r := range runs {
		res.Facts = append(res.Facts, Fact{S: r[0], T: r[1], Kind: "NoStmt", Parent: "Outside", Ordinal: -1, Depth: -1})
		if kinds[k] == "Doc" {
			doc = append(doc, r)
		} else {
			decl = append(decl, r)
		}
	}
	role := func(iv [][2]int) Role {
		if len(iv) == 0 {
			return Role{IV: [][2]int{}, Why: "structural_empty"}
		}
		return Role{IV: iv, Why: "present"}
	}
	res.Roles = map[string]Role{"CAND": role([][2]int{leaf}), "DECL": role(decl)}
	if h.Task == "RP" {
		res.Roles["DOC"] = role(doc)
	}
	return res
}

func packetEvent(h Header, e Event, state string, designated [2]string, kinds []string, runs [][2]int,
	leaf *[2]int) error {
	switch {
	case e.E == "BAD":
		return fmt.Errorf("bad event: %s", e.Reason)
	case e.N != "" || e.Single || e.Reason != "":
		return fmt.Errorf("%s carries a slot or reason field", e.E)
	case e.E != "CAND" && e.ID != "":
		return fmt.Errorf("%s carries a candidate id", e.E)
	}
	var run [2]int
	var kind string
	if n := len(runs); n > 0 {
		run, kind = runs[n-1], kinds[n-1]
	}
	switch state {
	case "between":
		if e.E != "OPEN" {
			return fmt.Errorf("event %s where the packet grammar requires OPEN", e.E)
		}
		next := append(slices.Clone(kinds), e.K)
		if !slices.ContainsFunc(packetVariants[h.Task], func(v []string) bool {
			return len(v) >= len(next) && slices.Equal(v[:len(next)], next)
		}) {
			return fmt.Errorf("run kinds %v begin no %s variant", next, h.Task)
		}
		if e.S < 0 || e.S >= e.T || e.T > h.Len {
			return fmt.Errorf("run [%d, %d) is empty or outside the %d-byte source", e.S, e.T, h.Len)
		}
		if len(runs) > 0 && e.S <= run[1] {
			return fmt.Errorf("run at %d does not start after a gap following the run ending at %d", e.S, run[1])
		}
	case "run":
		if kind == designated[0] {
			if e.E != "LEAF" || e.K != designated[1] {
				return fmt.Errorf("%s run requires a %s leaf, not %s %s", kind, designated[1], e.E, e.K)
			}
			whole := designated[1] == "Comment" && (e.S != run[0] || e.T != run[1])
			if whole || e.S < run[0] || e.S >= e.T || e.T != run[1] {
				return fmt.Errorf("leaf [%d, %d) is not the designated part of run %v", e.S, e.T, run)
			}
			*leaf = [2]int{e.S, e.T}
			return nil
		}
		return packetClose(e, kind)
	case "leaf":
		switch {
		case e.E != "CAND" || e.K != "":
			return fmt.Errorf("event %s %s where the packet grammar requires CAND", e.E, e.K)
		case e.ID != h.Cand:
			return fmt.Errorf("candidate id %q differs from header %q", e.ID, h.Cand)
		case [2]int{e.S, e.T} != *leaf:
			return fmt.Errorf("CAND [%d, %d) differs from the leaf %v", e.S, e.T, *leaf)
		}
	case "cand":
		return packetClose(e, kind)
	}
	return nil
}

func packetClose(e Event, kind string) error {
	switch {
	case e.E != "CLOSE":
		return fmt.Errorf("event %s where the packet grammar requires CLOSE", e.E)
	case e.K != kind:
		return fmt.Errorf("close kind %s does not match open %s", e.K, kind)
	case e.S != 0 || e.T != 0:
		return errors.New("CLOSE carries a span")
	}
	return nil
}
