package structural

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// --- direct AST-field oracle ------------------------------------------------
// The oracle reads go/ast fields directly and computes roles and per-byte facts from the specification formulas. It
// shares no code with the builder or the transducer.

type oracleCand struct {
	roots [][2]int
	roles map[string][][2]int
	facts map[int]Fact // per admitted byte; S and T unused
}

type oracleTree struct {
	src    []byte
	fset   *token.FileSet
	file   *ast.File
	parent map[ast.Node]ast.Node
	depth  map[ast.Node]int
	nodes  []ast.Node
	elses  []int
}

var oracleKinds = map[string]string{
	"*ast.DeclStmt": "Decl", "*ast.EmptyStmt": "Empty", "*ast.LabeledStmt": "Labeled",
	"*ast.ExprStmt": "Expr", "*ast.SendStmt": "Send", "*ast.IncDecStmt": "IncDec",
	"*ast.AssignStmt": "Assign", "*ast.GoStmt": "Go", "*ast.DeferStmt": "Defer",
	"*ast.ReturnStmt": "Return", "*ast.BranchStmt": "Branch", "*ast.BlockStmt": "Block",
	"*ast.IfStmt": "If", "*ast.CaseClause": "CaseClause", "*ast.SwitchStmt": "Switch",
	"*ast.TypeSwitchStmt": "TypeSwitch", "*ast.CommClause": "CommClause",
	"*ast.SelectStmt": "Select", "*ast.ForStmt": "For", "*ast.RangeStmt": "Range",
}

func newOracleTree(t *testing.T, src []byte) *oracleTree {
	t.Helper()
	o := &oracleTree{src: src, fset: token.NewFileSet(), parent: map[ast.Node]ast.Node{}, depth: map[ast.Node]int{}}
	file, err := parser.ParseFile(o.fset, "", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	o.file = file
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if len(stack) > 0 {
			o.parent[n] = stack[len(stack)-1]
		}
		o.depth[n] = len(stack)
		o.nodes = append(o.nodes, n)
		stack = append(stack, n)
		return true
	})
	var sc scanner.Scanner
	f := o.fset.AddFile("scan", -1, len(src))
	sc.Init(f, src, nil, scanner.ScanComments)
	for {
		pos, tok, _ := sc.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.ELSE {
			o.elses = append(o.elses, f.Offset(pos))
		}
	}
	return o
}

func (o *oracleTree) off(p token.Pos) int { return o.fset.Position(p).Offset }

func (o *oracleTree) span(n ast.Node) [2]int { return [2]int{o.off(n.Pos()), o.off(n.End())} }

func (o *oracleTree) regionsOf(n ast.Node) map[string][2]int {
	r := map[string][2]int{}
	switch n := n.(type) {
	case *ast.IfStmt:
		r["Then"] = o.span(n.Body)
		if n.Else != nil {
			r["Else"] = o.span(n.Else)
		}
	case *ast.ForStmt:
		r["LoopBody"] = o.span(n.Body)
	case *ast.RangeStmt:
		r["LoopBody"] = o.span(n.Body)
	case *ast.CaseClause:
		r["CaseBody"] = [2]int{o.off(n.Colon) + 1, o.off(n.End())}
	case *ast.CommClause:
		r["CaseBody"] = [2]int{o.off(n.Colon) + 1, o.off(n.End())}
	case *ast.FuncDecl:
		if n.Body != nil {
			r["FuncBody"] = o.span(n.Body)
		}
	case *ast.FuncLit:
		r["FuncBody"] = o.span(n.Body)
	}
	return r
}

func (o *oracleTree) isRegionBlock(b *ast.BlockStmt) bool {
	switch p := o.parent[b].(type) {
	case *ast.IfStmt:
		return p.Body == b || p.Else == b
	case *ast.ForStmt:
		return p.Body == b
	case *ast.RangeStmt:
		return p.Body == b
	case *ast.FuncDecl:
		return p.Body == b
	case *ast.FuncLit:
		return p.Body == b
	}
	return false
}

func (o *oracleTree) isFrame(n ast.Node) bool {
	switch n := n.(type) {
	case *ast.FuncDecl, *ast.FuncLit:
		return true
	case *ast.BlockStmt:
		return !o.isRegionBlock(n)
	}
	_, ok := oracleKinds[fmt.Sprintf("%T", n)]
	return ok
}

func (o *oracleTree) within(n, root ast.Node) bool {
	for ; n != nil; n = o.parent[n] {
		if n == root {
			return true
		}
	}
	return false
}

func (o *oracleTree) listPos(n, root ast.Node) (int, bool, bool) {
	if n == root || !o.within(o.parent[n], root) {
		return -1, false, false // the list lies in an omitted ancestor
	}
	var list []ast.Stmt
	switch p := o.parent[n].(type) {
	case *ast.BlockStmt:
		list = p.List
	case *ast.CaseClause:
		list = p.Body
	case *ast.CommClause:
		list = p.Body
	}
	for i, s := range list {
		if s == n {
			return i, i == len(list)-1, true
		}
	}
	return -1, false, false
}

// factsFor computes the facts of every admitted byte. root is the admitted node, and rootSpan is its admitted span (the
// doc envelope or the truncated signature).
func (o *oracleTree) factsFor(root ast.Node, rootSpan [2]int, into map[int]Fact) {
	var frames, owners []ast.Node
	for _, n := range o.nodes {
		if !o.within(n, root) {
			continue
		}
		if o.isFrame(n) {
			frames = append(frames, n)
		}
		if len(o.regionsOf(n)) > 0 {
			owners = append(owners, n)
		}
	}
	spanOf := func(n ast.Node) [2]int {
		if n == root {
			return rootSpan
		}
		return o.span(n)
	}
	for b := rootSpan[0]; b < rootSpan[1]; b++ {
		var owner ast.Node
		for _, n := range frames {
			s := spanOf(n)
			if s[0] <= b && b < s[1] && (owner == nil || o.depth[n] > o.depth[owner]) {
				owner = n
			}
		}
		f := Fact{Ordinal: -1, Depth: -1}
		_, fn := owner.(*ast.FuncDecl)
		if _, lit := owner.(*ast.FuncLit); lit {
			fn = true
		}
		if fn {
			f.Kind, f.Parent = "NoStmt", "FuncRoot"
		} else {
			f.Kind = oracleKinds[fmt.Sprintf("%T", owner)]
			f.Parent, f.Depth = "Outside", 0
			for p := o.parent[owner]; owner != root && p != nil; p = o.parent[p] {
				if !o.isFrame(p) {
					continue
				}
				switch p.(type) {
				case *ast.FuncDecl, *ast.FuncLit:
					if f.Parent == "Outside" {
						f.Parent = "FuncRoot"
					}
				default:
					if f.Parent == "Outside" {
						f.Parent = oracleKinds[fmt.Sprintf("%T", p)]
					}
					f.Depth++
				}
				if p == root {
					break
				}
			}
			if i, last, ok := o.listPos(owner, root); ok {
				f.Ordinal, f.Last = i, "NotLast"
				if last {
					f.Last = "Last"
				}
			}
		}
		best := -1
		for _, n := range owners {
			for name, r := range o.regionsOf(n) {
				if r[0] <= b && b < r[1] && o.depth[n] > best {
					best, f.Slot = o.depth[n], name
				}
				if _, lit := n.(*ast.FuncLit); lit && r[0] <= b && b < r[1] {
					f.Lit++
				}
			}
		}
		into[b] = f
	}
}

func (o *oracleTree) enclosingFunc(n ast.Node) ast.Node {
	for p := o.parent[n]; p != nil; p = o.parent[p] {
		switch p.(type) {
		case *ast.FuncDecl, *ast.FuncLit:
			return p
		}
	}
	return nil
}

func (o *oracleTree) sigSpan(fn ast.Node) [2]int {
	switch fn := fn.(type) {
	case *ast.FuncDecl:
		return o.span(fn.Type)
	case *ast.FuncLit:
		return o.span(fn.Type)
	}
	panic("not a function")
}

func oracleP1(t *testing.T, src []byte) []oracleCand {
	o := newOracleTree(t, src)
	type item struct {
		kw  int
		ifs *ast.IfStmt
	}
	var items []item
	for _, n := range o.nodes {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Else == nil {
			continue
		}
		lo, hi := o.off(ifs.Body.End()), o.off(ifs.Else.Pos())
		kw := -1
		for _, e := range o.elses {
			if lo <= e && e < hi {
				kw = e
			}
		}
		items = append(items, item{kw, ifs})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].kw < items[j].kw })
	var out []oracleCand
	for _, it := range items {
		fn := o.enclosingFunc(it.ifs)
		sig, root := o.sigSpan(fn), o.span(it.ifs)
		c := oracleCand{roots: [][2]int{sig, root}, facts: map[int]Fact{}, roles: map[string][][2]int{
			"CAND":      {{it.kw, it.kw + 4}},
			"IF_HDR":    {{root[0], o.off(it.ifs.Body.Lbrace)}},
			"THEN_BODY": {o.span(it.ifs.Body)},
			"THEN_LAST": {},
			"ELSE_BODY": {o.span(it.ifs.Else)},
			"FUNC_SIG":  {sig},
		}}
		if l := it.ifs.Body.List; len(l) > 0 {
			c.roles["THEN_LAST"] = [][2]int{o.span(l[len(l)-1])}
		}
		o.factsFor(fn, sig, c.facts)
		o.factsFor(it.ifs, root, c.facts)
		out = append(out, c)
	}
	return out
}

// --- oracle agreement -------------------------------------------------------

func fixtures(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, name := range []string{"p1", "ds", "kinds"} {
		src, err := os.ReadFile("testdata/" + name + ".txt")
		if err != nil {
			t.Fatal(err)
		}
		out[name] = src
		out[name+"-crlf"] = bytes.ReplaceAll(src, []byte("\n"), []byte("\r\n"))
	}
	return out
}

func roleIntervals(r Result) map[string][][2]int {
	out := map[string][][2]int{}
	for name, role := range r.Roles {
		iv := role.IV
		if iv == nil {
			iv = [][2]int{}
		}
		out[name] = iv
		want := "present"
		if len(iv) == 0 {
			want = "structural_empty"
		}
		if role.Why != want {
			out[name+"/why"] = nil
		}
	}
	return out
}

func TestSelectorAgreesWithDirectASTOracle(t *testing.T) {
	// visited collects statement kinds from compared facts, per line ending.
	visited := map[bool]map[string]bool{false: {}, true: {}}
	for name, src := range fixtures(t) {
		streams, err := Build("P1", name, src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := oracleP1(t, src)
		if len(streams) != len(want) || len(want) == 0 {
			t.Fatalf("%s: %d streams, oracle %d candidates", name, len(streams), len(want))
		}
		for k, s := range streams {
			got := Select(s.Header, s.Events)
			where := fmt.Sprintf("%s cand %d", name, k)
			if !got.OK {
				t.Fatalf("%s: selector failed: %s at event %v", where, got.Reason, got.EventIndex)
			}
			if wantID := fmt.Sprintf("P1:%s:%d", name, k); got.Cand != wantID {
				t.Errorf("%s: cand %q, want %q", where, got.Cand, wantID)
			}
			if !reflect.DeepEqual(got.Runs, want[k].roots) {
				t.Errorf("%s: runs %v, want %v", where, got.Runs, want[k].roots)
			}
			if roles := roleIntervals(got); !reflect.DeepEqual(roles, want[k].roles) {
				t.Errorf("%s: roles\n got %v\nwant %v", where, roles, want[k].roles)
			}
			checkFacts(t, where, got, want[k].facts)
			for _, f := range got.Facts {
				visited[strings.HasSuffix(name, "-crlf")][f.Kind] = true
			}
		}
	}
	for crlf, kinds := range visited {
		var missing []string
		for _, kind := range oracleKinds {
			if !kinds[kind] {
				missing = append(missing, kind)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("crlf=%v: compared projections never visit statement kinds %v", crlf, missing)
		}
	}
}

// Raw string values lose carriage returns, but spans must still cover the
// original source bytes.
func TestRawStringSpansFollowSourceBytes(t *testing.T) {
	lf := "package p\n\n// F doc.\nfunc F(a int) string {\n\tif a > 0 {\n\t\ts := `x\ny`\n\t\treturn `p\nq` + s\n" +
		"\t} else {\n\t\ta--\n\t}\n\tv := `m\nn`\n\treturn v + `r\ns`\n}\n"
	for _, crlf := range []bool{false, true} {
		src, nl := []byte(lf), "\n"
		if crlf {
			src, nl = bytes.ReplaceAll(src, []byte("\n"), []byte("\r\n")), "\r\n"
		}
		find := func(text string) [2]int {
			text = strings.ReplaceAll(text, "\n", nl)
			if bytes.Count(src, []byte(text)) != 1 {
				t.Fatalf("crlf=%v: %q not unique in fixture", crlf, text)
			}
			i := bytes.Index(src, []byte(text))
			return [2]int{i, i + len(text)}
		}
		thenAssign, thenRet := find("s := `x\ny`"), find("return `p\nq` + s")
		// owners gives the kind of each statement and the owner of the byte after it.
		owners := map[[2]int][2]string{
			thenAssign: {"Assign", "If"}, thenRet: {"Return", "If"},
		}
		streams, err := Build("P1", "raw", src)
		if err != nil || len(streams) != 1 {
			t.Fatalf("crlf=%v: %d streams, err %v", crlf, len(streams), err)
		}
		r := Select(streams[0].Header, streams[0].Events)
		if !r.OK {
			t.Fatalf("crlf=%v: %s", crlf, r.Reason)
		}
		want := [][2]int{thenRet}
		if got := r.Roles["THEN_LAST"].IV; !reflect.DeepEqual(got, want) {
			t.Errorf("crlf=%v: THEN_LAST %v, want %v", crlf, got, want)
		}
		for span, kinds := range owners {
			for i, b := range []int{span[0], span[1] - 1, span[1]} {
				want := kinds[0]
				if i == 2 {
					want = kinds[1]
				}
				if got, ok := factKind(r, b); !ok || got != want {
					t.Errorf("crlf=%v: byte %d (%q) owned by %q, want %q", crlf, b, src[b], got, want)
				}
			}
		}
	}
}

func factKind(r Result, b int) (string, bool) {
	for _, f := range r.Facts {
		if f.S <= b && b < f.T {
			return f.Kind, true
		}
	}
	return "", false
}

func checkFacts(t *testing.T, where string, got Result, want map[int]Fact) {
	t.Helper()
	seen := map[int]bool{}
	prev := -1
	for _, f := range got.Facts {
		if f.S < prev || f.S >= f.T {
			t.Fatalf("%s: facts not sorted, disjoint and non-empty at %v", where, f)
		}
		prev = f.T
		for b := f.S; b < f.T; b++ {
			w, ok := want[b]
			if !ok {
				t.Fatalf("%s: fact byte %d is not admitted", where, b)
			}
			g := f
			g.S, g.T = 0, 0
			if g != w {
				t.Fatalf("%s: byte %d: got %+v want %+v", where, b, g, w)
			}
			seen[b] = true
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("%s: facts cover %d bytes, admitted %d", where, len(seen), len(want))
	}
}

func TestBuildRejectsUnparseableSourceAndOtherTasks(t *testing.T) {
	if _, err := Build("P1", "x", []byte("package p\nfunc {")); err == nil {
		t.Fatal("unparseable source built")
	}
	for _, task := range []string{"DS", "A1"} {
		if _, err := Build(task, "x", []byte("package p\n")); err == nil {
			t.Errorf("task %s built", task)
		}
	}
}

// --- locality ---------------------------------------------------------------

// normalize rewrites every offset relative to the concatenated admitted runs. It drops the recovery metadata, which is
// allowed to differ.
func normalize(r Result) Result {
	at := func(o int) int {
		base := 0
		for _, run := range r.Runs {
			if run[0] <= o && o <= run[1] {
				return base + o - run[0]
			}
			base += run[1] - run[0]
		}
		return -1
	}
	out := Result{OK: r.OK, Task: r.Task, Roles: map[string]Role{}}
	for _, run := range r.Runs {
		out.Runs = append(out.Runs, [2]int{at(run[0]), at(run[1])})
	}
	for name, role := range r.Roles {
		n := Role{Why: role.Why, IV: [][2]int{}}
		for _, iv := range role.IV {
			n.IV = append(n.IV, [2]int{at(iv[0]), at(iv[1])})
		}
		out.Roles[name] = n
	}
	for _, f := range r.Facts {
		f.S, f.T = at(f.S), at(f.T)
		out.Facts = append(out.Facts, f)
	}
	return out
}

// admitted splits src into its admitted runs, joined by one separator, and the omitted bytes around and between them.
func admitted(src []byte, r Result) (in, out string) {
	var parts []string
	at := 0
	for _, run := range r.Runs {
		parts = append(parts, string(src[run[0]:run[1]]))
		out += string(src[at:run[0]])
		at = run[1]
	}
	return strings.Join(parts, "\x00"), out + string(src[at:])
}

func only(t *testing.T, task string, src []byte, want string) Result {
	t.Helper()
	streams, err := Build(task, "loc", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range streams {
		r := Select(s.Header, s.Events)
		if !r.OK {
			t.Fatalf("selector failed: %s", r.Reason)
		}
		if strings.Contains(string(src[r.Runs[len(r.Runs)-1][0]:r.Runs[len(r.Runs)-1][1]]), want) {
			return r
		}
	}
	t.Fatalf("no candidate admits %q", want)
	return Result{}
}

func TestResultsAreLocalToAdmittedBytes(t *testing.T) {
	cases := []struct{ task, a, b, marker string }{
		{"P1",
			"package p\n\nfunc F(a int) int {\n\tif a > 0 {\n\t\treturn 1\n\t} else {\n\t\ta--\n\t}\n\treturn a\n}\n",
			"package p\n\n// Doc differs.\nfunc G() {}\n\nfunc F(a int) int {\n\tfor i := 0; i < 3; i++ {\n\t\ta += i\n\t}\n" +
				"\tswitch {\n\tcase a > 9:\n\tif a > 0 {\n\t\treturn 1\n\t} else {\n\t\ta--\n\t}\n\t}\n\treturn a\n}\n",
			"a--"},
	}
	for _, c := range cases {
		ra, rb := only(t, c.task, []byte(c.a), c.marker), only(t, c.task, []byte(c.b), c.marker)
		ina, outa := admitted([]byte(c.a), ra)
		inb, outb := admitted([]byte(c.b), rb)
		if ina != inb || outa == outb {
			t.Fatalf("%s: fixture precondition: admitted bytes must match and omitted bytes differ\n"+
				" a %q | %q\n b %q | %q", c.task, ina, outa, inb, outb)
		}
		if na, nb := normalize(ra), normalize(rb); !reflect.DeepEqual(na, nb) {
			t.Errorf("%s: results differ outside admitted bytes\n a %+v\n b %+v", c.task, na, nb)
		}
	}
}

// --- malformed streams ------------------------------------------------------

func p1Stream() []Event {
	return []Event{
		{E: "OPEN", K: "FuncDecl", S: 0, T: 10},
		{E: "LEAF", K: "Name", S: 5, T: 6},
		{E: "LEAF", K: "Params", S: 6, T: 8},
		{E: "CLOSE", K: "FuncDecl"},
		{E: "OPEN", K: "If", S: 20, T: 60},
		{E: "SLOT", N: "Then", S: 25, T: 35},
		{E: "OPEN", K: "Return", S: 27, T: 33},
		{E: "CLOSE", K: "Return"},
		{E: "LEAF", K: "ElseKw", S: 36, T: 40},
		{E: "CAND", ID: "P1:m:0", S: 36, T: 40},
		{E: "SLOT", N: "Else", S: 41, T: 60},
		{E: "CLOSE", K: "If"},
	}
}

func edit(events []Event, at, drop int, insert ...Event) []Event {
	out := append([]Event{}, events[:at]...)
	out = append(out, insert...)
	return append(out, events[at+drop:]...)
}

func TestHandwrittenStreamsAreAccepted(t *testing.T) {
	r := Select(Header{Task: "P1", Cand: "P1:m:0", Len: 100}, p1Stream())
	if !r.OK {
		t.Fatalf("P1: %s at %v", r.Reason, *r.EventIndex)
	}
	if got := r.Roles["THEN_LAST"].IV; !reflect.DeepEqual(got, [][2]int{{27, 33}}) {
		t.Errorf("THEN_LAST %v", got)
	}
	if got := r.Roles["IF_HDR"].IV; !reflect.DeepEqual(got, [][2]int{{20, 25}}) {
		t.Errorf("IF_HDR %v", got)
	}
}

// Statement depth crosses blocks and literals but resets at each admitted root.
// Function frames themselves do not count as statements.
func TestDepthCountsStatementFramesAndResetsAtRoots(t *testing.T) {
	depthAt := func(r Result, b int) (int, int) {
		for _, f := range r.Facts {
			if f.S <= b && b < f.T {
				return f.Depth, f.Lit
			}
		}
		t.Fatalf("byte %d has no fact", b)
		return 0, 0
	}
	r := Select(Header{Task: "P1", Cand: "P1:m:0", Len: 100}, p1Stream())
	if !r.OK {
		t.Fatalf("P1: %s at %v", r.Reason, *r.EventIndex)
	}
	for b, want := range map[int]int{0: -1, 20: 0, 27: 1} {
		if d, _ := depthAt(r, b); d != want {
			t.Errorf("P1 byte %d: depth %d, want %d", b, d, want)
		}
	}

	// A function literal sits in the Then list: Return [27, 33) holds FuncLit [28, 33), whose body [29, 33) holds Block
	// [30, 32) and Expr [31, 32).
	nested := edit(p1Stream(), 7, 0,
		Event{E: "OPEN", K: "FuncLit", S: 28, T: 33},
		Event{E: "SLOT", N: "FuncBody", S: 29, T: 33},
		Event{E: "OPEN", K: "Block", S: 30, T: 32},
		Event{E: "OPEN", K: "Expr", S: 31, T: 32},
		Event{E: "CLOSE", K: "Expr"},
		Event{E: "CLOSE", K: "Block"},
		Event{E: "CLOSE", K: "FuncLit"},
	)
	r = Select(Header{Task: "P1", Cand: "P1:m:0", Len: 100}, nested)
	if !r.OK {
		t.Fatalf("nested: %s at %v", r.Reason, *r.EventIndex)
	}
	for b, want := range map[int][2]int{20: {0, 0}, 27: {1, 0}, 28: {-1, 0}, 30: {2, 1}, 31: {3, 1}} {
		if d, lit := depthAt(r, b); d != want[0] || lit != want[1] {
			t.Errorf("nested byte %d: depth %d lit %d, want %v", b, d, lit, want)
		}
	}
}

func TestMalformedStreamsFailWholeCandidate(t *testing.T) {
	p1 := p1Stream()
	cases := []struct {
		name, task string
		events     []Event
		reason     string
	}{
		{"bad event", "P1", edit(p1, 6, 0, Event{E: "BAD", Reason: "else keyword not located"}), "bad event"},
		{"unmatched close", "P1", append(append([]Event{}, p1...), Event{E: "CLOSE", K: "Return"}), "close without open"},
		{"mis-kinded close", "P1", edit(p1, 7, 1, Event{E: "CLOSE", K: "If"}), "close kind"},
		{"overlapping siblings", "P1", edit(p1, 8, 0, Event{E: "OPEN", K: "Return", S: 28, T: 34}, Event{E: "CLOSE", K: "Return"}), "overlap"},
		{"child outside parent", "P1", edit(p1, 6, 2, Event{E: "OPEN", K: "Return", S: 50, T: 65}, Event{E: "CLOSE", K: "Return"}), "outside"},
		{"duplicate slot", "P1", edit(p1, 10, 0, Event{E: "SLOT", N: "Then", S: 41, T: 50}), "duplicate slot"},
		{"slot not allowed", "P1", edit(p1, 5, 0, Event{E: "SLOT", N: "LoopBody", S: 21, T: 24}), "slot not allowed"},
		{"slot out of order", "P1", edit(p1, 5, 0, Event{E: "SLOT", N: "Else", S: 21, T: 24}), "slot order"},
		{"entry outside region", "P1", edit(p1, 6, 2, Event{E: "OPEN", K: "Return", S: 34, T: 36}, Event{E: "CLOSE", K: "Return"}), "outside slot region"},
		{"leaf outside frame", "P1", edit(p1, 3, 0, Event{E: "LEAF", K: "Results", S: 12, T: 13}), "outside"},
		{"leaf inside region", "P1", edit(p1, 8, 1, Event{E: "LEAF", K: "ElseKw", S: 33, T: 35}), "inside slot region"},
		{"no candidate", "P1", edit(p1, 9, 1), "no candidate"},
		{"two candidates", "P1", edit(p1, 10, 0, Event{E: "CAND", ID: "P1:m:0", S: 36, T: 40}), "candidate"},
		{"candidate span differs from leaf", "P1", edit(p1, 9, 1, Event{E: "CAND", ID: "P1:m:0", S: 36, T: 39}), "candidate must follow"},
		{"candidate on wrong leaf", "P1", edit(p1, 2, 0, Event{E: "CAND", ID: "P1:m:0", S: 5, T: 6}), "candidate"},
		{"candidate id mismatch", "P1", edit(p1, 9, 1, Event{E: "CAND", ID: "P1:m:9", S: 36, T: 40}), "candidate id"},
		{"literal without body", "P1", edit(p1, 7, 0, Event{E: "OPEN", K: "FuncLit", S: 28, T: 32}, Event{E: "CLOSE", K: "FuncLit"}), "FuncBody"},
		{"container block short", "P1", edit(p1, 6, 2, Event{E: "OPEN", K: "Switch", S: 26, T: 34}, Event{E: "OPEN", K: "Block", S: 28, T: 32},
			Event{E: "CLOSE", K: "Block"}, Event{E: "CLOSE", K: "Switch"}), "container"},
		{"switch without container", "P1", edit(p1, 6, 2, Event{E: "OPEN", K: "Switch", S: 26, T: 34}, Event{E: "CLOSE", K: "Switch"}), "container"},
		{"unclosed frame", "P1", p1[:len(p1)-1], "unclosed"},
		{"three P1 roots", "P1", append(append([]Event{}, p1...), Event{E: "OPEN", K: "If", S: 70, T: 80}, Event{E: "CLOSE", K: "If"}), "root"},
		{"P1 without signature root", "P1", p1[4:], "root"},
		{"single else not one If", "P1", edit(p1, 10, 1, Event{E: "SLOT", N: "Else", S: 41, T: 60, Single: true},
			Event{E: "OPEN", K: "Block", S: 41, T: 60}, Event{E: "CLOSE", K: "Block"}), "single"},
		{"offset beyond source", "P1", edit(p1, 0, 1, Event{E: "OPEN", K: "FuncDecl", S: 0, T: 150}), "offset"},
		{"retired task", "DS", p1, "unknown task"},
	}
	for _, c := range cases {
		r := Select(Header{Task: c.task, Cand: c.task + ":m:0", Len: 100}, c.events)
		if r.OK || !strings.Contains(r.Reason, c.reason) {
			t.Errorf("%s: ok=%v reason %q, want failure containing %q", c.name, r.OK, r.Reason, c.reason)
			continue
		}
		if r.Runs != nil || r.Roles != nil || r.Facts != nil || r.EventIndex == nil {
			t.Errorf("%s: failure carries runs, roles or facts, or no event index: %+v", c.name, r)
		}
	}
}
