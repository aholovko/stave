package structural

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/aholovko/stave/internal/astinfo"
)

// piece is a source span for an interface owner header, attached doc block or
// declaration signature. Bodies and fields are excluded from these packets.
type piece struct {
	kind string
	s, t int
}

type packetCand struct {
	pieces []piece
	cand   [2]int // DC: the Doc run; RP: the result list inside the Decl run
}

// BuildDCRP returns DC or RP candidate streams in source order, plus withheld counts
// by reason. IDs have the form <task>:<id>:<cand start>-<cand end>.
//
// DC pairs attached prose with exported struct or method declarations. RP selects
// exported signatures with wholly unnamed results and repeated written result types.
// Ambiguous documentation, aliases and inseparable packet pieces are withheld rather
// than treated as checked negatives.
// Withheld reasons are alias, shared_doc, no_prose and adjacent_pieces.
func BuildDCRP(task, id string, src []byte) ([]Stream, map[string]int, error) {
	if _, ok := packetVariants[task]; !ok {
		return nil, nil, fmt.Errorf("unknown DC/RP task %q", task)
	}
	if !utf8.Valid(src) {
		return nil, nil, errors.New("source is not valid UTF-8")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, astinfo.ParserMode)
	if err != nil {
		return nil, nil, err
	}
	b := &builder{src: src, fset: fset}
	withheld := map[string]int{}
	var cands []packetCand
	add := func(reason string, c packetCand) {
		for k := 1; reason == "" && k < len(c.pieces); k++ {
			if c.pieces[k].s <= c.pieces[k-1].t {
				reason = "adjacent_pieces"
			}
		}
		if reason != "" {
			withheld[reason]++
			return
		}
		cands = append(cands, c)
	}
	doc := func(g *ast.CommentGroup) piece {
		sp := astinfo.CommentGroupSpan(src, fset, g)
		return piece{"Doc", sp.Start, sp.End}
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			sig := piece{"Decl", b.off(d.Pos()), b.off(d.Type.End())}
			switch {
			case task == "DC" && d.Recv != nil && d.Doc != nil:
				dp := doc(d.Doc)
				add(noProse(d.Doc), packetCand{[]piece{dp, sig}, [2]int{dp.s, dp.t}})
			case task == "RP" && repeatedResults(fset, d.Type):
				ps := []piece{sig}
				if d.Doc != nil {
					ps = []piece{doc(d.Doc), sig}
				}
				add("", packetCand{ps, b.span(d.Type.Results)})
			}
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				t := spec.(*ast.TypeSpec)
				start := b.off(d.Pos())
				if d.Lparen.IsValid() {
					start = b.off(t.Pos())
				}
				alias := ""
				if t.Assign.IsValid() {
					alias = "alias"
				}
				switch x := t.Type.(type) {
				case *ast.StructType:
					if task != "DC" || !t.Name.IsExported() {
						continue
					}
					g, reason := t.Doc, alias
					if g == nil && d.Doc != nil {
						if len(d.Specs) == 1 {
							g = d.Doc
						} else if reason == "" {
							reason = "shared_doc"
						}
					}
					if g == nil {
						if reason != "" && d.Doc != nil {
							withheld[reason]++
						}
						continue
					}
					if reason == "" {
						reason = noProse(g)
					}
					dp := doc(g)
					hdr := piece{"Decl", start, b.off(x.Fields.Opening) + 1}
					add(reason, packetCand{[]piece{dp, hdr}, [2]int{dp.s, dp.t}})
				case *ast.InterfaceType:
					owner := piece{"Owner", start, b.off(x.Methods.Opening) + 1}
					for _, m := range x.Methods.List {
						ft, ok := m.Type.(*ast.FuncType)
						if !ok || len(m.Names) != 1 || !m.Names[0].IsExported() {
							continue
						}
						sig := piece{"Decl", b.off(m.Pos()), b.off(m.End())}
						switch {
						case task == "DC" && m.Doc != nil:
							reason := alias
							if reason == "" {
								reason = noProse(m.Doc)
							}
							dp := doc(m.Doc)
							add(reason, packetCand{[]piece{owner, dp, sig}, [2]int{dp.s, dp.t}})
						case task == "RP" && repeatedResults(fset, ft):
							ps := []piece{owner, sig}
							if m.Doc != nil {
								ps = []piece{owner, doc(m.Doc), sig}
							}
							add(alias, packetCand{ps, b.span(ft.Results)})
						}
					}
				}
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].cand[0] < cands[j].cand[0] })
	sum := sha256.Sum256(src)
	header := Header{Task: task, Len: len(src), SHA256: hex.EncodeToString(sum[:])}
	leaf := packetLeaf[task]
	streams := make([]Stream, 0, len(cands))
	for _, c := range cands {
		header.Cand = fmt.Sprintf("%s:%s:%d-%d", task, id, c.cand[0], c.cand[1])
		var ev []Event
		for _, p := range c.pieces {
			ev = append(ev, Event{E: "OPEN", K: p.kind, S: p.s, T: p.t})
			if p.kind == leaf[0] {
				ev = append(ev, Event{E: "LEAF", K: leaf[1], S: c.cand[0], T: c.cand[1]},
					Event{E: "CAND", ID: header.Cand, S: c.cand[0], T: c.cand[1]})
			}
			ev = append(ev, Event{E: "CLOSE", K: p.kind})
		}
		streams = append(streams, Stream{Header: header, Events: ev})
	}
	return streams, withheld, nil
}

// Directives are not documentation prose; CommentGroup.Text removes them.
func noProse(g *ast.CommentGroup) string {
	if strings.TrimSpace(g.Text()) == "" {
		return "no_prose"
	}
	return ""
}

// Compare formatted syntax so whitespace does not make identical written types
// look different. This check does not resolve aliases or infer type identity.
func repeatedResults(fset *token.FileSet, ft *ast.FuncType) bool {
	if ft.Results == nil || len(ft.Results.List) < 2 {
		return false
	}
	seen := map[string]bool{}
	repeated := false
	for _, f := range ft.Results.List {
		if len(f.Names) > 0 {
			return false
		}
		var buf bytes.Buffer
		if err := format.Node(&buf, fset, f.Type); err != nil {
			return false
		}
		repeated = repeated || seen[buf.String()]
		seen[buf.String()] = true
	}
	return repeated
}
