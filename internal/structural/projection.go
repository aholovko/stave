package structural

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"sort"
	"unicode/utf8"

	"github.com/aholovko/stave/internal/astinfo"
	"github.com/aholovko/stave/internal/tfcand"
)

// Build returns a P1 stream per else-owning if statement, in source order.
// Candidate IDs have the form P1:<id>:<k>. The builder supplies syntax and spans;
// roles and ownership are derived separately by Select. Invalid UTF-8 or syntax is an error.
func Build(task, id string, src []byte) ([]Stream, error) {
	if task != "P1" {
		return nil, fmt.Errorf("unknown task %q", task)
	}
	if !utf8.Valid(src) {
		return nil, errors.New("source is not valid UTF-8")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, astinfo.ParserMode)
	if err != nil {
		return nil, err
	}
	b := &builder{src: src, fset: fset}
	type pending struct {
		at   int
		emit func()
	}
	var cands []pending
	var stack, fns []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			if isFunc(stack[len(stack)-1]) {
				fns = fns[:len(fns)-1]
			}
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		if isFunc(n) {
			fns = append(fns, n)
		}
		if s, ok := n.(*ast.IfStmt); ok && s.Else != nil {
			fn := fns[len(fns)-1]
			at, found := b.elseKw(s)
			if !found {
				at = b.off(s.Body.End())
			}
			cands = append(cands, pending{at, func() { b.cand = s; b.sigRoot(fn); b.stmt(s) }})
		}
		return true
	})
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].at < cands[j].at })
	sum := sha256.Sum256(src)
	header := Header{Task: task, Len: len(src), SHA256: hex.EncodeToString(sum[:])}
	streams := make([]Stream, 0, len(cands))
	for k, c := range cands {
		header.Cand = fmt.Sprintf("%s:%s:%d", task, id, k)
		b.events, b.candID = nil, header.Cand
		c.emit()
		streams = append(streams, Stream{Header: header, Events: b.events})
	}
	return streams, nil
}

func isFunc(n ast.Node) bool {
	switch n.(type) {
	case *ast.FuncDecl, *ast.FuncLit:
		return true
	}
	return false
}

type builder struct {
	src      []byte
	fset     *token.FileSet
	events   []Event
	cand     ast.Node
	candID   string
	inserted int // TF: CAND insertions in the current stream
	tf       *tfcand.Occurrence
	runArgs  map[*ast.FuncLit]tfcand.RunArg // TF: Run-argument literals of the root
	root     [2]int                         // TF: the doc-inclusive root span
}

func (b *builder) off(p token.Pos) int { return b.fset.PositionFor(p, false).Offset }

func (b *builder) emit(e Event) { b.events = append(b.events, e) }

func (b *builder) open(kind string, n ast.Node) {
	b.emit(Event{E: "OPEN", K: kind, S: b.off(n.Pos()), T: b.off(n.End())})
}

func (b *builder) close(kind string) { b.emit(Event{E: "CLOSE", K: kind}) }

func (b *builder) leaf(kind string, n ast.Node) {
	b.emit(Event{E: "LEAF", K: kind, S: b.off(n.Pos()), T: b.off(n.End())})
}

func (b *builder) slot(name string, s, t int, single bool) {
	b.emit(Event{E: "SLOT", N: name, S: s, T: t, Single: single})
}

func (b *builder) region(name string, n ast.Node, single bool) {
	b.slot(name, b.off(n.Pos()), b.off(n.End()), single)
}

// sigRoot emits the P1 signature root: the function truncated to its signature, with no body slot.
func (b *builder) sigRoot(fn ast.Node) {
	switch fn := fn.(type) {
	case *ast.FuncDecl:
		b.emit(Event{E: "OPEN", K: "FuncDecl", S: b.off(fn.Type.Pos()), T: b.off(fn.Type.End())})
		b.signature(fn.Recv, fn.Name, fn.Type)
		b.close("FuncDecl")
	case *ast.FuncLit:
		b.emit(Event{E: "OPEN", K: "FuncLit", S: b.off(fn.Type.Pos()), T: b.off(fn.Type.End())})
		b.signature(nil, nil, fn.Type)
		b.close("FuncLit")
	}
}

func (b *builder) signature(recv *ast.FieldList, name *ast.Ident, typ *ast.FuncType) {
	if recv != nil {
		b.leaf("Recv", recv)
	}
	if name != nil {
		b.leaf("Name", name)
	}
	b.leaf("Params", typ.Params)
	if typ.Results != nil {
		b.leaf("Results", typ.Results)
	}
}

func (b *builder) funcLit(lit *ast.FuncLit) {
	if ra, ok := b.runArgs[lit]; ok {
		b.mark("RunName", b.off(ra.Name.Pos()), b.off(ra.Name.End()))
		s, t := b.ownerSpan(ra.Owner)
		b.mark("RunBind", s, t)
	}
	b.open("FuncLit", lit)
	b.signature(nil, nil, lit.Type)
	b.region("FuncBody", lit.Body, false)
	b.stmts(lit.Body.List)
	b.close("FuncLit")
}

func (b *builder) lits(nodes ...ast.Node) {
	for _, n := range nodes {
		if n == nil {
			continue
		}
		ast.Inspect(n, func(m ast.Node) bool {
			if lit, ok := m.(*ast.FuncLit); ok {
				b.funcLit(lit)
				return false
			}
			return true
		})
	}
}

func (b *builder) stmts(list []ast.Stmt) {
	for _, s := range list {
		b.stmt(s)
	}
}

var simpleKinds = map[string]string{
	"*ast.DeclStmt": "Decl", "*ast.EmptyStmt": "Empty", "*ast.ExprStmt": "Expr", "*ast.SendStmt": "Send",
	"*ast.IncDecStmt": "IncDec", "*ast.AssignStmt": "Assign", "*ast.GoStmt": "Go", "*ast.DeferStmt": "Defer",
	"*ast.ReturnStmt": "Return", "*ast.BranchStmt": "Branch",
}

func (b *builder) stmt(s ast.Stmt) {
	if s == nil {
		return
	}
	switch s := s.(type) {
	case *ast.BlockStmt:
		b.open("Block", s)
		b.stmts(s.List)
		b.close("Block")
	case *ast.IfStmt:
		b.open("If", s)
		b.stmt(s.Init)
		b.lits(s.Cond)
		b.region("Then", s.Body, false)
		b.stmts(s.Body.List)
		if s.Else != nil {
			if at, ok := b.elseKw(s); ok {
				b.emit(Event{E: "LEAF", K: "ElseKw", S: at, T: at + len("else")})
				if b.cand == ast.Node(s) {
					b.emit(Event{E: "CAND", ID: b.candID, S: at, T: at + len("else")})
				}
			} else {
				b.emit(Event{E: "BAD", Reason: "else keyword not located"})
			}
			switch e := s.Else.(type) {
			case *ast.IfStmt:
				b.region("Else", e, true)
				b.stmt(e)
			case *ast.BlockStmt:
				b.region("Else", e, false)
				b.stmts(e.List)
			}
		}
		b.close("If")
	case *ast.ForStmt:
		b.open("For", s)
		b.stmt(s.Init)
		b.lits(s.Cond)
		b.stmt(s.Post)
		b.region("LoopBody", s.Body, false)
		b.stmts(s.Body.List)
		b.close("For")
	case *ast.RangeStmt:
		b.open("Range", s)
		b.lits(s.Key, s.Value, s.X)
		b.region("LoopBody", s.Body, false)
		b.stmts(s.Body.List)
		b.close("Range")
	case *ast.SwitchStmt:
		b.open("Switch", s)
		b.stmt(s.Init)
		b.lits(s.Tag)
		b.stmt(s.Body)
		b.close("Switch")
	case *ast.TypeSwitchStmt:
		b.open("TypeSwitch", s)
		b.stmt(s.Init)
		b.stmt(s.Assign)
		b.stmt(s.Body)
		b.close("TypeSwitch")
	case *ast.SelectStmt:
		b.open("Select", s)
		b.stmt(s.Body)
		b.close("Select")
	case *ast.CaseClause:
		b.open("CaseClause", s)
		for _, e := range s.List {
			b.lits(e)
		}
		b.slot("CaseBody", b.off(s.Colon)+1, b.off(s.End()), false)
		b.stmts(s.Body)
		b.close("CaseClause")
	case *ast.CommClause:
		b.open("CommClause", s)
		b.stmt(s.Comm)
		b.slot("CaseBody", b.off(s.Colon)+1, b.off(s.End()), false)
		b.stmts(s.Body)
		b.close("CommClause")
	case *ast.LabeledStmt:
		b.open("Labeled", s)
		b.stmt(s.Stmt)
		b.close("Labeled")
	default:
		kind, ok := simpleKinds[fmt.Sprintf("%T", s)]
		if !ok {
			b.emit(Event{E: "BAD", Reason: fmt.Sprintf("unsupported statement %T", s)})
			return
		}
		b.open(kind, s)
		if b.tf != nil && s == ast.Stmt(b.tf.Stmt) {
			b.tfCand()
		}
		b.lits(s)
		b.close(kind)
	}
}

// The AST has no else-keyword position, so scan the gap between its two branches.
// Only an ELSE token may occur there.
func (b *builder) elseKw(s *ast.IfStmt) (int, bool) {
	from, to := b.off(s.Body.End()), b.off(s.Else.Pos())
	f := token.NewFileSet().AddFile("", -1, to-from)
	var sc scanner.Scanner
	sc.Init(f, b.src[from:to], nil, 0)
	at := -1
	for {
		pos, tok, _ := sc.Scan()
		if tok == token.EOF {
			return from + at, at >= 0
		}
		if tok != token.ELSE || at >= 0 {
			return 0, false
		}
		at = f.Offset(pos)
	}
}
