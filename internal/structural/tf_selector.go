package structural

import (
	"errors"
	"fmt"
	"sort"
)

// TF markers describe arguments and bindings without owning bytes, so they cannot
// change ownership facts. CandArgs/CandBind follow CAND; RunName/RunBind precede
// the FuncLit they describe.
var markKinds = map[string]bool{"CandArgs": true, "CandBind": true, "RunName": true, "RunBind": true}

// tfHeaders are the frame kinds whose completed headers enter ENCL_HDR.
var tfHeaders = map[string]bool{
	"If": true, "For": true, "Range": true, "Switch": true, "TypeSwitch": true, "Select": true,
	"CaseClause": true, "CommClause": true,
}

// runPair is the Run name interval of a FuncLit and its receiver-binding owner.
type runPair struct {
	name [2]int
	bind *frame
}

// tfState is the TF marker sequence and the role registers set at CAND.
type tfState struct {
	want    string // the next required event: a marker kind or FuncLit
	args    [2]int
	bind    *frame
	prev    [][2]int
	hdr     [][2]int
	runName [2]int
	runBind *frame
}

func (x *selector) tfSequence(e *Event) error {
	if e.E == "MARK" && !markKinds[e.K] {
		return fmt.Errorf("unknown marker kind %q", e.K)
	}
	switch x.tf.want {
	case "":
		if e.E == "MARK" && e.K != "RunName" {
			return fmt.Errorf("marker %s out of sequence", e.K)
		}
	case "FuncLit":
		if e.E != "OPEN" || e.K != "FuncLit" {
			return errors.New("a Run marker pair must immediately precede a FuncLit OPEN")
		}
	default:
		if e.E != "MARK" || e.K != x.tf.want {
			return fmt.Errorf("expected marker %s", x.tf.want)
		}
	}
	return nil
}

// fnFrame returns the open root or FuncLit frame that spans exactly e.
func (x *selector) fnFrame(e *Event) *frame {
	for _, f := range x.stack {
		if (f.root && f.kind == "FuncDecl" || f.kind == "FuncLit") && f.s == e.S && f.t == e.T {
			return f
		}
	}
	return nil
}

func (x *selector) mark(e *Event) error {
	if x.h.Task != "TF" {
		return errors.New("MARK events only in TF streams")
	}
	f := x.top()
	if f == nil {
		return errors.New("marker outside any frame")
	}
	if err := x.span(e); err != nil {
		return err
	}
	switch e.K {
	case "CandArgs":
		if e.S >= e.T || e.S <= x.cand[0] || e.T >= x.cand[1] {
			return fmt.Errorf("CandArgs [%d, %d) is empty or not strictly inside CAND", e.S, e.T)
		}
		x.tf.args, x.tf.want = [2]int{e.S, e.T}, "CandBind"
	case "CandBind":
		if x.tf.bind = x.fnFrame(e); x.tf.bind == nil {
			return errors.New("CandBind must equal an open root or FuncLit frame")
		}
		x.tf.want = ""
	case "RunName":
		if e.S >= e.T || e.S < f.s || e.T > f.t {
			return fmt.Errorf("RunName [%d, %d) is empty or outside its enclosing %s", e.S, e.T, f.kind)
		}
		x.tf.runName, x.tf.want = [2]int{e.S, e.T}, "RunBind"
	case "RunBind":
		if x.tf.runBind = x.fnFrame(e); x.tf.runBind == nil {
			return errors.New("RunBind must equal an open root or FuncLit frame")
		}
		x.tf.want = "FuncLit"
	}
	return nil
}

func (x *selector) attachRun(e *Event) error {
	f := x.top()
	if x.tf.runName[1] > e.S {
		return errors.New("RunName must end by the literal start")
	}
	if x.tf.runBind.level >= f.level {
		return errors.New("RunBind owner must strictly enclose its literal")
	}
	f.run = &runPair{name: x.tf.runName, bind: x.tf.runBind}
	x.tf.want, x.tf.runBind = "", nil
	return nil
}

// tfCandidate binds the CAND to the Expr frame that the previous event opened. It records PREV and the enclosing
// headers that are complete at that point.
func (x *selector) tfCandidate(e *Event) error {
	f, p := x.top(), x.prev
	if f == nil || f.kind != "Expr" || p == nil || p.E != "OPEN" || p.K != "Expr" || p.S != e.S || p.T != e.T ||
		f.s != e.S || f.t != e.T {
		return errors.New("TF candidate must immediately follow the OPEN of its Expr frame")
	}
	x.bound, x.cand, x.candSeen = x.roots[0], [2]int{e.S, e.T}, true
	x.tf.want = "CandArgs"
	if f.d.ordinal > 0 && f.listFirst >= 0 {
		x.tf.prev = [][2]int{{f.listFirst, f.s}}
	}
	for _, g := range x.stack[:len(x.stack)-1] {
		if tfHeaders[g.kind] && g.hasHdr && g.hdr[1] <= e.S {
			x.tf.hdr = append(x.tf.hdr, g.hdr)
		}
	}
	return nil
}

// tfRoot checks the TF root at its CLOSE. The root needs keyword, name and parameters leaves, and a candidate. It has a
// Doc leaf exactly when it starts before the keyword.
func (x *selector) tfRoot(f *frame) error {
	for _, k := range []string{"FuncKw", "Name", "Params"} {
		if _, ok := f.leaves[k]; !ok {
			return fmt.Errorf("TF root lacks a %s leaf", k)
		}
	}
	_, doc := f.leaves["Doc"]
	if doc != (f.s < f.leaves["FuncKw"][0]) {
		return errors.New("TF root must start at its Doc leaf, or at FuncKw without one")
	}
	if !x.candSeen {
		return errors.New("TF root lacks a candidate")
	}
	return nil
}

func union(ivs [][2]int) [][2]int {
	s := append([][2]int(nil), ivs...)
	sort.Slice(s, func(i, j int) bool { return s[i][0] < s[j][0] })
	var out [][2]int
	for _, iv := range s {
		if n := len(out); n > 0 && iv[0] <= out[n-1][1] {
			out[n-1][1] = max(out[n-1][1], iv[1])
			continue
		}
		out = append(out, iv)
	}
	return out
}

// RUN_PREFIX follows binding owners rather than lexical nesting: a nested callback
// may use an outer testing parameter. Stop at the root or a literal without a Run pair.
func (x *selector) tfRoles(f *frame, ivs map[string][][2]int) error {
	end := f.leaves["Params"][1]
	if r, ok := f.leaves["Results"]; ok {
		end = r[1]
	}
	sig := [2]int{f.leaves["FuncKw"][0], end}
	body := f.regions["FuncBody"]
	var prefix [][2]int
	for g := x.tf.bind; !g.root && g.run != nil; g = g.run.bind {
		if g.run.bind.level >= g.level {
			return errors.New("RUN_PREFIX owner chain does not strictly ascend")
		}
		prefix = append(prefix, g.run.name)
	}
	bind := sig
	if b := x.tf.bind; !b.root {
		bind = [2]int{b.s, b.regions["FuncBody"].s}
	}
	ivs["ARGS"] = [][2]int{x.tf.args}
	ivs["PREV"] = x.tf.prev
	ivs["ENCL_HDR"] = union(x.tf.hdr)
	ivs["RUN_PREFIX"] = union(prefix)
	ivs["BIND_SIG"] = [][2]int{bind}
	ivs["FUNC_SIG"] = [][2]int{sig}
	ivs["BODY"] = [][2]int{{body.s, body.t}}
	return nil
}
