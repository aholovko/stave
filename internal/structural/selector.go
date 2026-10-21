package structural

import (
	"errors"
	"fmt"
)

var stmtKinds = map[string]bool{
	"Decl": true, "Empty": true, "Labeled": true, "Expr": true, "Send": true, "IncDec": true, "Assign": true,
	"Go": true, "Defer": true, "Return": true, "Branch": true, "Block": true, "If": true, "CaseClause": true,
	"Switch": true, "TypeSwitch": true, "CommClause": true, "Select": true, "For": true, "Range": true,
}

// slotNames lists each frame kind's slots in their required order.
var slotNames = map[string][]string{
	"If": {"Then", "Else"}, "For": {"LoopBody"}, "Range": {"LoopBody"}, "CaseClause": {"CaseBody"},
	"CommClause": {"CaseBody"}, "FuncDecl": {"FuncBody"}, "FuncLit": {"FuncBody"},
}

var leafOwners = map[string]map[string]bool{
	"Doc": {"FuncDecl": true}, "Recv": {"FuncDecl": true}, "Name": {"FuncDecl": true},
	"Params": {"FuncDecl": true, "FuncLit": true}, "Results": {"FuncDecl": true, "FuncLit": true},
	"ElseKw": {"If": true}, "FuncKw": {"FuncDecl": true},
}

func isSwitch(kind string) bool { return kind == "Switch" || kind == "TypeSwitch" || kind == "Select" }

// desc is the descriptor of a frame. Every owned interval of the frame shares it. The Last flag of a list entry is set
// once, through this reference, when its list ends. Ordinal and depth are negative for None.
type desc struct {
	kind, parent   string
	ordinal, depth int
	last           string
	s, t           int
}

type owned struct {
	s, t int
	d    *desc
	slot string
	lit  int
}

type region struct {
	name   string
	s, t   int
	single bool
	count  int
	last   *desc
	first  int // start of the list's first entry (TF PREV)
}

type frame struct {
	kind      string
	fn, root  bool
	s, t      int
	d         *desc
	lit       int    // literal depth at OPEN
	stmts     int    // statement frames on the stack from the root through this frame
	slot      string // enclosing slot context
	cursor    int    // end of the bytes already owned or given to children and leaves
	live      *region
	regions   map[string]*region
	count     int   // Block frame entries
	last      *desc // Block frame's latest entry
	leaves    map[string][2]int
	hdr       [2]int
	container bool
	hasHdr    bool     // hdr is complete
	first     int      // Block frame: start of its own list's first entry
	listFirst int      // start of the first entry of the list holding this frame, or -1
	level     int      // stack depth at OPEN
	run       *runPair // TF: the Run marker pair preceding this FuncLit
}

type selector struct {
	h            Header
	stack, roots []*frame
	owned        []owned
	prev         *Event
	prevFrame    *frame
	bound        *frame
	cand         [2]int
	candSeen     bool
	roles        map[string]Role
	tf           tfState // TF: marker sequence and role registers
}

// Select runs the transducer over one stream. Any violated check fails the whole candidate. The result then carries the
// reason and the event index, and no runs, roles or facts. Event index -1 is the header, and len(events) is the end of
// the stream.
func Select(h Header, events []Event) Result {
	res := Result{Task: h.Task, Cand: h.Cand, Source: Source{Bytes: h.Len, SHA256: h.SHA256}}
	fail := func(i int, err error) Result {
		res.Reason, res.EventIndex = err.Error(), &i
		return res
	}
	if _, ok := envelopeKinds[h.Task]; ok {
		return selectEnvelope(h, events)
	}
	if _, ok := packetVariants[h.Task]; ok {
		return selectPacket(h, events)
	}
	if h.Task != "P1" && h.Task != "TF" {
		return fail(-1, fmt.Errorf("unknown task %q", h.Task))
	}
	x := &selector{h: h}
	for i := range events {
		if err := x.step(&events[i]); err != nil {
			return fail(i, err)
		}
		x.prev = &events[i]
	}
	if err := x.finish(); err != nil {
		return fail(len(events), err)
	}
	res.OK, res.Roles = true, x.roles
	for _, r := range x.roots {
		res.Runs = append(res.Runs, [2]int{r.s, r.t})
	}
	for _, o := range x.owned {
		res.Facts = append(res.Facts, Fact{S: o.s, T: o.t, Kind: o.d.kind, Parent: o.d.parent, Slot: o.slot,
			Ordinal: o.d.ordinal, Last: o.d.last, Depth: o.d.depth, Lit: o.lit})
	}
	return res
}

func (x *selector) step(e *Event) error {
	if x.h.Task == "TF" {
		if err := x.tfSequence(e); err != nil {
			return err
		}
	}
	switch e.E {
	case "BAD":
		return fmt.Errorf("bad event: %s", e.Reason)
	case "OPEN":
		if err := x.open(e); err != nil {
			return err
		}
		if x.tf.want == "FuncLit" {
			return x.attachRun(e)
		}
		return nil
	case "MARK":
		return x.mark(e)
	case "CLOSE":
		return x.close(e)
	case "SLOT":
		return x.slot(e)
	case "LEAF":
		return x.leaf(e)
	case "CAND":
		return x.candidate(e)
	}
	return fmt.Errorf("unknown event %q", e.E)
}

func (x *selector) top() *frame {
	if len(x.stack) == 0 {
		return nil
	}
	return x.stack[len(x.stack)-1]
}

func (x *selector) span(e *Event) error {
	if e.S < 0 || e.S > e.T || e.T > x.h.Len {
		return fmt.Errorf("offsets [%d, %d) are not within the %d-byte source", e.S, e.T, x.h.Len)
	}
	return nil
}

func (x *selector) open(e *Event) error {
	fn := e.K == "FuncDecl" || e.K == "FuncLit"
	if !fn && !stmtKinds[e.K] {
		return fmt.Errorf("unknown frame kind %q", e.K)
	}
	if err := x.span(e); err != nil {
		return err
	}
	if e.S == e.T && e.K != "Empty" {
		return fmt.Errorf("zero-length %s frame", e.K)
	}
	f := &frame{kind: e.K, fn: fn, s: e.S, t: e.T, cursor: e.S, regions: map[string]*region{},
		leaves: map[string][2]int{}, listFirst: -1, level: len(x.stack)}
	d := &desc{kind: e.K, parent: "FuncRoot", ordinal: -1, depth: -1, s: e.S, t: e.T}
	if fn {
		d.kind = "NoStmt"
	}
	f.d = d
	p := x.top()
	if p == nil {
		if err := x.openRoot(f); err != nil {
			return err
		}
		if !fn {
			d.parent, d.depth, f.stmts = "Outside", 0, 1
		}
		x.stack = append(x.stack, f)
		return nil
	}
	if e.S < p.cursor {
		return fmt.Errorf("%s at %d overlaps an earlier child or leaf of %s", e.K, e.S, p.kind)
	}
	if e.T > p.t {
		return fmt.Errorf("%s [%d, %d) lies outside its parent %s", e.K, e.S, e.T, p.kind)
	}
	if e.K == "FuncDecl" {
		return errors.New("nested FuncDecl")
	}
	f.lit, f.slot = p.lit, p.slot
	if r := p.live; r != nil {
		if e.S < r.s || e.T > r.t {
			return fmt.Errorf("%s [%d, %d) lies outside slot region %s", e.K, e.S, e.T, r.name)
		}
		f.slot = r.name
		if p.kind == "FuncLit" && r.name == "FuncBody" {
			f.lit++
		}
		switch {
		case r.single:
			if r.count > 0 || e.K != "If" || e.S != r.s || e.T != r.t {
				return errors.New("single Else slot requires one If spanning its region")
			}
			r.count++
		case fn:
			return fmt.Errorf("function literal directly in slot region %s", r.name)
		default:
			if r.count == 0 {
				r.first = e.S
			}
			d.ordinal, d.last, f.listFirst = r.count, "NotLast", r.first
			r.count++
			r.last = d
		}
	} else if p.kind == "Block" {
		if fn {
			return errors.New("function literal directly in a Block")
		}
		if p.count == 0 {
			p.first = e.S
		}
		d.ordinal, d.last, f.listFirst = p.count, "NotLast", p.first
		p.count++
		p.last = d
	}
	if (e.K == "CaseClause" || e.K == "CommClause") && p.kind != "Block" {
		return fmt.Errorf("%s outside a container Block", e.K)
	}
	if e.K == "Block" && isSwitch(p.kind) {
		if p.container || e.T != p.t {
			return fmt.Errorf("container Block of %s must be single and end at the frame end", p.kind)
		}
		p.container = true
		x.header(p, e.S)
	}
	if err := x.own(p, e.S); err != nil {
		return err
	}
	f.stmts = p.stmts
	if !fn {
		if !p.fn {
			d.parent = p.kind
		}
		d.depth = p.stmts
		f.stmts++
	}
	x.stack = append(x.stack, f)
	return nil
}

func (x *selector) openRoot(f *frame) error {
	n := len(x.roots)
	switch {
	case n > 0 && f.s < x.roots[n-1].t:
		return errors.New("roots overlap or are out of order")
	case x.h.Task == "TF" && (n > 0 || f.kind != "FuncDecl"):
		return errors.New("TF stream must have exactly one FuncDecl root")
	case x.h.Task == "P1" && n == 0 && !f.fn:
		return errors.New("P1 first root must be a function signature")
	case x.h.Task == "P1" && n == 1 && f.kind != "If":
		return errors.New("P1 second root must be the owning If")
	case x.h.Task == "P1" && n > 1:
		return errors.New("P1 stream has more than two roots")
	}
	f.root = true
	x.roots = append(x.roots, f)
	return nil
}

// Split ownership at slot boundaries so each byte retains the correct slot facts.
func (x *selector) own(f *frame, to int) error {
	if to < f.cursor {
		return fmt.Errorf("event at %d overlaps bytes of %s ending at %d", to, f.kind, f.cursor)
	}
	if r := f.live; r != nil && f.cursor < r.t {
		end := min(to, r.t)
		lit := f.lit
		if f.kind == "FuncLit" && r.name == "FuncBody" {
			lit++
		}
		x.add(f, f.cursor, end, r.name, lit)
		f.cursor = end
	}
	if f.cursor < to {
		if f.live != nil {
			if err := x.endSlot(f); err != nil {
				return err
			}
		}
		x.add(f, f.cursor, to, f.slot, f.lit)
		f.cursor = to
	}
	return nil
}

func (x *selector) add(f *frame, s, t int, slot string, lit int) {
	if s < t {
		x.owned = append(x.owned, owned{s: s, t: t, d: f.d, slot: slot, lit: lit})
	}
}

// A list entry is known to be last only when its slot closes.
func (x *selector) endSlot(f *frame) error {
	r := f.live
	f.live = nil
	if r.single {
		if r.count != 1 {
			return errors.New("single Else slot requires one If spanning its region")
		}
		return nil
	}
	if r.last != nil {
		r.last.last = "Last"
	}
	return nil
}

// The body start determines where its enclosing header ends.
func (x *selector) header(f *frame, at int) {
	f.hdr, f.hasHdr = [2]int{f.s, at}, true
}

func (x *selector) slot(e *Event) error {
	f := x.top()
	if f == nil {
		return errors.New("slot outside any frame")
	}
	names := slotNames[f.kind]
	pos := -1
	for i, n := range names {
		if n == e.N {
			pos = i
		}
	}
	switch {
	case pos < 0:
		return fmt.Errorf("slot not allowed: %s on %s", e.N, f.kind)
	case f.regions[e.N] != nil:
		return fmt.Errorf("duplicate slot %s on %s", e.N, f.kind)
	case pos > 0 && f.regions[names[pos-1]] == nil:
		return fmt.Errorf("slot order: %s before %s on %s", e.N, names[pos-1], f.kind)
	case e.Single && e.N != "Else":
		return fmt.Errorf("single slot %s is not Else", e.N)
	}
	if err := x.span(e); err != nil {
		return err
	}
	if e.S < f.cursor || e.T > f.t {
		return fmt.Errorf("slot %s [%d, %d) lies outside %s or before its earlier children", e.N, e.S, e.T, f.kind)
	}
	if r := f.live; r != nil && e.S < r.t {
		return fmt.Errorf("slot %s overlaps slot %s", e.N, r.name)
	}
	if err := x.own(f, e.S); err != nil {
		return err
	}
	if f.live != nil {
		if err := x.endSlot(f); err != nil {
			return err
		}
	}
	r := &region{name: e.N, s: e.S, t: e.T, single: e.Single}
	f.regions[e.N], f.live = r, r
	switch {
	case e.N == "Then" || e.N == "LoopBody":
		x.header(f, e.S)
	case e.N == "CaseBody" && x.h.Task == "TF":
		f.hdr, f.hasHdr = [2]int{f.s, e.S}, true
	}
	return nil
}

func (x *selector) leaf(e *Event) error {
	f := x.top()
	if f == nil {
		return errors.New("leaf outside any frame")
	}
	if e.K == "FuncKw" && (x.h.Task != "TF" || !f.root || e.T-e.S != len("func")) {
		return errors.New("FuncKw leaf must be the four-byte keyword of the TF root")
	}
	if !leafOwners[e.K][f.kind] {
		return fmt.Errorf("leaf %s not allowed on %s", e.K, f.kind)
	}
	if err := x.span(e); err != nil {
		return err
	}
	switch {
	case e.S == e.T:
		return fmt.Errorf("empty leaf %s", e.K)
	case e.S < f.s || e.T > f.t:
		return fmt.Errorf("leaf %s [%d, %d) lies outside frame %s", e.K, e.S, e.T, f.kind)
	case f.leaves[e.K] != [2]int{}:
		return fmt.Errorf("duplicate leaf %s", e.K)
	case f.live != nil && e.S < f.live.t:
		return fmt.Errorf("leaf %s lies inside slot region %s", e.K, f.live.name)
	case e.S < f.cursor:
		return fmt.Errorf("leaf %s overlaps an earlier child or leaf", e.K)
	}
	if err := x.own(f, e.S); err != nil {
		return err
	}
	if f.live != nil {
		if err := x.endSlot(f); err != nil {
			return err
		}
	}
	switch {
	case e.K == "ElseKw" && (f.regions["Then"] == nil || f.regions["Else"] != nil):
		return errors.New("ElseKw leaf must lie between the Then and Else slots")
	case e.K == "Doc" && (!f.root || e.S != f.s):
		return errors.New("Doc leaf must open the root declaration")
	}
	if err := x.own(f, e.T); err != nil {
		return err
	}
	f.leaves[e.K] = [2]int{e.S, e.T}
	x.prevFrame = f
	return nil
}

func (x *selector) candidate(e *Event) error {
	if x.candSeen {
		return errors.New("second candidate in one stream")
	}
	if e.ID != x.h.Cand {
		return fmt.Errorf("candidate id %q differs from header %q", e.ID, x.h.Cand)
	}
	if x.h.Task == "TF" {
		return x.tfCandidate(e)
	}
	f, p := x.top(), x.prev
	if f == nil || p == nil || p.E != "LEAF" || p.S != e.S || p.T != e.T || x.prevFrame != f {
		return errors.New("candidate must follow a leaf of identical span on the innermost frame")
	}
	const want = "ElseKw"
	if p.K != want || !f.root {
		return fmt.Errorf("candidate must follow the %s leaf of the root, not %s on %s", want, p.K, f.kind)
	}
	x.bound, x.cand, x.candSeen = f, [2]int{e.S, e.T}, true
	return nil
}

func (x *selector) close(e *Event) error {
	f := x.top()
	if f == nil {
		return errors.New("close without open")
	}
	if f.kind != e.K {
		return fmt.Errorf("close kind %s does not match open %s", e.K, f.kind)
	}
	if err := x.own(f, f.t); err != nil {
		return err
	}
	if f.live != nil {
		if err := x.endSlot(f); err != nil {
			return err
		}
	}
	has := func(name string) bool { return f.regions[name] != nil }
	switch f.kind {
	case "If":
		_, kw := f.leaves["ElseKw"]
		if !has("Then") || has("Else") != kw {
			return errors.New("If needs Then, and Else exactly with an ElseKw leaf")
		}
	case "For", "Range":
		if !has("LoopBody") {
			return fmt.Errorf("%s without LoopBody", f.kind)
		}
	case "CaseClause", "CommClause":
		if !has("CaseBody") {
			return fmt.Errorf("%s without CaseBody", f.kind)
		}
	case "Switch", "TypeSwitch", "Select":
		if !f.container {
			return fmt.Errorf("%s missing container Block", f.kind)
		}
	case "Block":
		if f.last != nil {
			f.last.last = "Last"
		}
	case "FuncDecl", "FuncLit":
		signature := f.root && x.h.Task == "P1" && x.roots[0] == f
		if signature == has("FuncBody") {
			return fmt.Errorf("%s: only the P1 signature root may lack a FuncBody", f.kind)
		}
		if f.root && x.h.Task == "TF" {
			if err := x.tfRoot(f); err != nil {
				return err
			}
		}
	}
	x.stack = x.stack[:len(x.stack)-1]
	if p := x.top(); p != nil {
		p.cursor = f.t
	}
	if f == x.bound {
		return x.emit(f)
	}
	return nil
}

// Role registers are fixed at frame close, after all owned intervals are known.
func (x *selector) emit(f *frame) error {
	ivs := map[string][][2]int{"CAND": {x.cand}}
	bounds := map[string][2]int{}
	switch x.h.Task {
	case "P1":
		then, els := f.regions["Then"], f.regions["Else"]
		ivs["IF_HDR"] = [][2]int{f.hdr}
		ivs["THEN_BODY"] = [][2]int{{then.s, then.t}}
		ivs["THEN_LAST"] = nil
		if then.last != nil {
			ivs["THEN_LAST"] = [][2]int{{then.last.s, then.last.t}}
		}
		ivs["ELSE_BODY"] = [][2]int{{els.s, els.t}}
		sig := x.roots[0]
		ivs["FUNC_SIG"] = [][2]int{{sig.s, sig.t}}
		bounds["FUNC_SIG"] = [2]int{sig.s, sig.t}
	case "TF":
		if err := x.tfRoles(f, ivs); err != nil {
			return err
		}
	}
	x.roles = map[string]Role{}
	for name, list := range ivs {
		lo, hi := f.s, f.t
		if b, ok := bounds[name]; ok {
			lo, hi = b[0], b[1]
		}
		for _, iv := range list {
			if iv[0] < lo || iv[1] > hi || iv[0] >= iv[1] {
				return fmt.Errorf("role %s interval %v escapes [%d, %d)", name, iv, lo, hi)
			}
		}
		role := Role{IV: list, Why: "present"}
		if len(list) == 0 {
			role = Role{IV: [][2]int{}, Why: "structural_empty"}
		}
		x.roles[name] = role
	}
	return nil
}

// Require complete, balanced coverage of the admitted runs before exposing features.
func (x *selector) finish() error {
	if f := x.top(); f != nil {
		return fmt.Errorf("unclosed frame %s at end of stream", f.kind)
	}
	if !x.candSeen {
		return errors.New("no candidate in stream")
	}
	if x.tf.want != "" {
		return fmt.Errorf("incomplete marker sequence awaiting %s", x.tf.want)
	}
	if want := map[string]int{"P1": 2, "TF": 1}[x.h.Task]; len(x.roots) != want {
		return fmt.Errorf("%s stream has %d roots, want %d", x.h.Task, len(x.roots), want)
	}
	i := 0
	for _, r := range x.roots {
		at := r.s
		for ; i < len(x.owned) && x.owned[i].s < r.t; i++ {
			if x.owned[i].s != at {
				return fmt.Errorf("partition gap or overlap at byte %d", at)
			}
			at = x.owned[i].t
		}
		if at != r.t {
			return fmt.Errorf("partition gap at byte %d", at)
		}
	}
	if i != len(x.owned) {
		return errors.New("owned bytes lie outside the runs")
	}
	return nil
}
