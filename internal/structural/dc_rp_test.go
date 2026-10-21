package structural_test

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/aholovko/stave/internal/structural"
)

const dcSource = "package p\n\n" +
	"// Ünïcode T holds things.\n" +
	"type T struct {\n\tA int\n}\n\n" +
	"// M does m.\r\n" +
	"func (t *t) M() {}\n\n" +
	"func (T) N() {}\n\n" +
	"// F is free.\nfunc F() {}\n\n" +
	"// C is a constant.\nconst C = 1\n\n" +
	"type i interface {\n\t// Get gets.\n\tGet() int\n\tfmt.Stringer\n}\n\n" +
	"// Shared doc.\ntype (\n\tA struct{}\n\tB struct{}\n)\n\n" +
	"// Al is an alias.\ntype Al = struct{}\n\n" +
	"//go:generate x\ntype D struct{}\n"

const rpSource = "package p\n\n" +
	"// Pair returns two.\nfunc Pair() (int, int) { return 0, 0 }\n\n" +
	"func Three() (bool, error, bool) { return false, nil, false }\n\n" +
	"func Named() (a, b int) { return }\n\n" +
	"func Distinct() (int, error) { return 0, nil }\n\n" +
	"func (x *t) Meth() (error, error) { return nil, nil }\n\n" +
	"func lower() (int, int) { return 0, 0 }\n\n" +
	"type R interface {\n\t// Get gets.\n\tGet() ([]byte, []byte)\n\tPut() (int, int)\n}\n\n" +
	"type Q = interface{ Z() (int, int) }\n"

// packet is one selected candidate: the CAND text, the run texts and the role texts, each in source order.
type packet struct {
	cand  string
	runs  []string
	roles map[string][]string
}

func texts(src string, ivs [][2]int) []string {
	out := []string{}
	for _, iv := range ivs {
		out = append(out, src[iv[0]:iv[1]])
	}
	return out
}

func selectAll(t *testing.T, task, src string) ([]packet, []structural.Result, map[string]int) {
	t.Helper()
	streams, withheld, err := structural.BuildDCRP(task, "fx", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var out []packet
	var results []structural.Result
	for _, s := range streams {
		r := structural.Select(s.Header, s.Events)
		if !r.OK {
			t.Fatalf("%s failed at %v: %s", s.Header.Cand, r.EventIndex, r.Reason)
		}
		p := packet{cand: texts(src, r.Roles["CAND"].IV)[0], runs: texts(src, r.Runs), roles: map[string][]string{}}
		for name, role := range r.Roles {
			p.roles[name] = texts(src, role.IV)
		}
		out = append(out, p)
		results = append(results, r)
	}
	return out, results, withheld
}

func packetSHA(runs []string) string {
	sum := sha256.Sum256([]byte(strings.Join(runs, "\x00")))
	return hex.EncodeToString(sum[:])
}

func TestDCEnumeratesStructsMethodsAndInterfaceMethods(t *testing.T) {
	got, results, withheld := selectAll(t, "DC", dcSource)
	want := []packet{
		{"// Ünïcode T holds things.", []string{"// Ünïcode T holds things.", "type T struct {"},
			map[string][]string{"CAND": {"// Ünïcode T holds things."}, "DECL": {"type T struct {"}}},
		{"// M does m.", []string{"// M does m.", "func (t *t) M()"},
			map[string][]string{"CAND": {"// M does m."}, "DECL": {"func (t *t) M()"}}},
		{"// Get gets.", []string{"type i interface {", "// Get gets.", "Get() int"},
			map[string][]string{"CAND": {"// Get gets."}, "DECL": {"type i interface {", "Get() int"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DC packets\n got %q\nwant %q", got, want)
	}
	if w := map[string]int{"shared_doc": 2, "alias": 1, "no_prose": 1}; !reflect.DeepEqual(withheld, w) {
		t.Errorf("withheld %v, want %v", withheld, w)
	}
	// Check the original UTF-8 byte offsets, the stable span ID and one neutral fact per run.
	start := strings.Index(dcSource, "// Ünïcode")
	end := start + len("// Ünïcode T holds things.")
	r := results[0]
	if r.Cand != "DC:fx:"+strconv.Itoa(start)+"-"+strconv.Itoa(end) || r.Roles["CAND"].IV[0] != [2]int{start, end} {
		t.Errorf("cand %s %v, want span [%d, %d)", r.Cand, r.Roles["CAND"].IV, start, end)
	}
	for _, r := range results {
		if len(r.Facts) != len(r.Runs) {
			t.Fatalf("%s: %d facts for %d runs", r.Cand, len(r.Facts), len(r.Runs))
		}
		for k, f := range r.Facts {
			want := structural.Fact{S: r.Runs[k][0], T: r.Runs[k][1], Kind: "NoStmt", Parent: "Outside", Ordinal: -1, Depth: -1}
			if f != want {
				t.Errorf("%s fact %d = %+v", r.Cand, k, f)
			}
		}
	}
	if packetSHA(got[0].runs) != packetSHA([]string{"// Ünïcode T holds things.", "type T struct {"}) {
		t.Error("struct packet digest differs from the helper shape")
	}
}

func TestDCGroupedSpecDocAndSingleGroupedFallback(t *testing.T) {
	src := "package p\n\ntype (\n\t// A doc.\n\tA struct{}\n\tB struct{}\n)\n\n// S doc.\ntype (\n\tS struct{ x int }\n)\n"
	got, _, withheld := selectAll(t, "DC", src)
	if len(got) != 2 || got[0].roles["DECL"][0] != "A struct{" || got[1].cand != "// S doc." ||
		got[1].roles["DECL"][0] != "S struct{" || len(withheld) != 0 {
		t.Fatalf("got %q withheld %v", got, withheld)
	}
}

func TestRPEnumeratesRepeatedUnnamedResults(t *testing.T) {
	got, _, withheld := selectAll(t, "RP", rpSource)
	empty := []string{}
	want := []packet{
		{"(int, int)", []string{"// Pair returns two.", "func Pair() (int, int)"},
			map[string][]string{"CAND": {"(int, int)"}, "DECL": {"func Pair() (int, int)"}, "DOC": {"// Pair returns two."}}},
		{"(bool, error, bool)", []string{"func Three() (bool, error, bool)"},
			map[string][]string{"CAND": {"(bool, error, bool)"}, "DECL": {"func Three() (bool, error, bool)"}, "DOC": empty}},
		{"(error, error)", []string{"func (x *t) Meth() (error, error)"},
			map[string][]string{"CAND": {"(error, error)"}, "DECL": {"func (x *t) Meth() (error, error)"}, "DOC": empty}},
		{"([]byte, []byte)", []string{"type R interface {", "// Get gets.", "Get() ([]byte, []byte)"},
			map[string][]string{"CAND": {"([]byte, []byte)"}, "DECL": {"type R interface {", "Get() ([]byte, []byte)"},
				"DOC": {"// Get gets."}}},
		{"(int, int)", []string{"type R interface {", "Put() (int, int)"},
			map[string][]string{"CAND": {"(int, int)"}, "DECL": {"type R interface {", "Put() (int, int)"}, "DOC": empty}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RP packets\n got %q\nwant %q", got, want)
	}
	if w := map[string]int{"alias": 1}; !reflect.DeepEqual(withheld, w) {
		t.Errorf("withheld %v, want %v", withheld, w)
	}
}

func TestDCRPRejectsUnknownTaskAndUnparseableSource(t *testing.T) {
	if _, _, err := structural.BuildDCRP("P2", "fx", []byte(dcSource)); err == nil {
		t.Error("P2 accepted by the DC/RP builder")
	}
	if _, _, err := structural.BuildDCRP("DC", "fx", []byte("package p\nfunc (")); err == nil {
		t.Error("unparseable source accepted")
	}
}

func TestDCRPSelectorRejectsTamperedStreams(t *testing.T) {
	streams, _, err := structural.BuildDCRP("RP", "fx", []byte(rpSource))
	if err != nil {
		t.Fatal(err)
	}
	var plain, owner structural.Stream // Three: one Decl run; Get: Owner, Doc, Decl
	for _, s := range streams {
		switch len(s.Events) {
		case 4:
			if plain.Events == nil {
				plain = s
			}
		case 8:
			owner = s
		}
	}
	if plain.Events == nil || owner.Events == nil {
		t.Fatal("fixture streams missing")
	}
	clone := func(s structural.Stream) (structural.Header, []structural.Event) {
		return s.Header, append([]structural.Event(nil), s.Events...)
	}
	cases := []struct {
		name   string
		base   structural.Stream
		mutate func(h *structural.Header, ev *[]structural.Event)
		index  int
	}{
		{"empty header cand", plain, func(h *structural.Header, _ *[]structural.Event) { h.Cand = "" }, -1},
		{"cand id differs", plain, func(_ *structural.Header, ev *[]structural.Event) { (*ev)[2].ID = "RP:fx:0-1" }, 2},
		{"cand differs from leaf", plain, func(_ *structural.Header, ev *[]structural.Event) { (*ev)[2].S++ }, 2},
		{"leaf before run end", plain, func(_ *structural.Header, ev *[]structural.Event) { (*ev)[1].T-- }, 1},
		{"leaf outside run", plain, func(_ *structural.Header, ev *[]structural.Event) { (*ev)[1].S = (*ev)[0].S - 1 }, 1},
		{"run beyond source", plain, func(h *structural.Header, ev *[]structural.Event) { (*ev)[0].T = h.Len + 1 }, 0},
		{"empty run", plain, func(_ *structural.Header, ev *[]structural.Event) { (*ev)[0].T = (*ev)[0].S }, 0},
		{"wrong leaf kind", plain, func(_ *structural.Header, ev *[]structural.Event) { (*ev)[1].K = "Comment" }, 1},
		{"close span", plain, func(_ *structural.Header, ev *[]structural.Event) { (*ev)[3].S = 1 }, 3},
		{"short stream", plain, func(_ *structural.Header, ev *[]structural.Event) { *ev = (*ev)[:3] }, 3},
		{"trailing event", plain, func(_ *structural.Header, ev *[]structural.Event) {
			*ev = append(*ev, structural.Event{E: "CLOSE", K: "Decl"})
		}, 4},
		{"bad event", plain, func(_ *structural.Header, ev *[]structural.Event) {
			(*ev)[1] = structural.Event{E: "BAD", Reason: "x"}
		}, 1},
		{"slot field", plain, func(_ *structural.Header, ev *[]structural.Event) { (*ev)[0].N = "Then" }, 0},
		{"owner after decl", owner, func(_ *structural.Header, ev *[]structural.Event) {
			(*ev)[0].S, (*ev)[0].T = (*ev)[6].T+1, (*ev)[6].T+2
		}, 2},
		{"owner overlaps doc", owner, func(_ *structural.Header, ev *[]structural.Event) { (*ev)[0].T = (*ev)[2].S + 1 }, 2},
		{"adjacent runs", owner, func(_ *structural.Header, ev *[]structural.Event) { (*ev)[0].T = (*ev)[2].S }, 2},
		{"variant not enumerated", owner, func(_ *structural.Header, ev *[]structural.Event) {
			(*ev)[2].K, (*ev)[3].K = "Owner", "Owner"
		}, 2},
		{"leaf on doc run", owner, func(_ *structural.Header, ev *[]structural.Event) {
			*ev = append((*ev)[:3:3], append([]structural.Event{{E: "LEAF", K: "Results", S: (*ev)[2].S, T: (*ev)[2].T}},
				(*ev)[3:]...)...)
		}, 3},
		{"second cand", owner, func(_ *structural.Header, ev *[]structural.Event) {
			*ev = append((*ev)[:7:7], (*ev)[6], (*ev)[7])
		}, 7},
		{"task tampered", plain, func(h *structural.Header, _ *[]structural.Event) { h.Task = "DC" }, 0},
	}
	for _, c := range cases {
		h, ev := clone(c.base)
		c.mutate(&h, &ev)
		r := structural.Select(h, ev)
		if r.OK || r.EventIndex == nil || *r.EventIndex != c.index || r.Runs != nil || r.Roles != nil || r.Facts != nil {
			idx := -9
			if r.EventIndex != nil {
				idx = *r.EventIndex
			}
			t.Errorf("%s: ok=%v index=%d (want %d) reason=%q", c.name, r.OK, idx, c.index, r.Reason)
		}
	}
	// Untampered streams still select.
	for _, s := range []structural.Stream{plain, owner} {
		if r := structural.Select(s.Header, s.Events); !r.OK {
			t.Errorf("%s: %s", s.Header.Cand, r.Reason)
		}
	}
}
