package structural

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/aholovko/stave/internal/astinfo"
	"github.com/aholovko/stave/internal/pkgsnap"
)

// Ledger records every P2 or P3 occurrence, including unstreamed ones. Statuses,
// reasons and certificate details stay here so they cannot leak into model features.
type Ledger struct {
	Task        string                      `json:"task"`
	Source      LedgerSource                `json:"source"`
	Certificate *pkgsnap.CertificateSummary `json:"certificate,omitempty"`
	Records     []Record                    `json:"records"`
}

// LedgerSource is the source identity the ledger is bound to.
type LedgerSource struct {
	ID     string `json:"id"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Record is one ledger record. Cand and Envelope are set exactly for streamed records. Reasons lists every applicable
// condition in precedence order, and the first one is the Status.
type Record struct {
	ID               string   `json:"id"`
	Status           string   `json:"status"`
	Reasons          []string `json:"reasons"`
	Anchor           [2]int   `json:"anchor"`
	Cand             *[2]int  `json:"cand,omitempty"`
	Envelope         *[2]int  `json:"envelope,omitempty"`
	Base             string   `json:"base,omitempty"`
	Name             string   `json:"name,omitempty"`
	Subtype          string   `json:"subtype,omitempty"`
	Import           *[2]int  `json:"import,omitempty"`
	FileConflicts    [][2]int `json:"file_conflicts,omitempty"`
	PackageConflicts []string `json:"package_conflicts,omitempty"`
}

var p2Callees = map[string]string{"errors": "New", "fmt": "Errorf"}

var envelopeKinds = map[string][2]string{"P2": {"CallEnv", "Msg"}, "P3": {"DeclEnv", "DeclName"}}

// BuildLedger returns the P2 or P3 ledger and streams for records with candidate spans,
// in ledger order. IDs have the form <task>:<id>:<k>. A verified P2 certificate
// must bind src; a nil or zero certificate means unknown context. P3 takes no certificate.
func BuildLedger(task, id string, src []byte, cert *pkgsnap.Certificate) ([]Stream, *Ledger, error) {
	if _, ok := envelopeKinds[task]; !ok {
		return nil, nil, fmt.Errorf("unknown ledger task %q", task)
	}
	if task == "P3" && cert != nil {
		return nil, nil, errors.New("P3 takes no package certificate")
	}
	if cert.Verified() {
		if err := cert.CheckTarget(src); err != nil {
			return nil, nil, err
		}
	}
	if !utf8.Valid(src) {
		return nil, nil, errors.New("source is not valid UTF-8")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, astinfo.ParserMode)
	if err != nil {
		return nil, nil, err
	}
	return ledgerFile(task, id, fset, file, src, cert)
}

type entry struct {
	rec Record
	pre int
}

// Accept an already parsed file so integrity tests can exercise malformed ASTs
// that the parser itself would never return.
func ledgerFile(task, id string, fset *token.FileSet, file *ast.File, src []byte, cert *pkgsnap.Certificate) ([]Stream, *Ledger, error) {
	b := &builder{src: src, fset: fset}
	var entries []entry
	var err error
	if task == "P2" {
		entries, err = b.p2(file, cert)
	} else {
		entries, err = b.p3(file)
	}
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, c := entries[i].rec.Anchor, entries[j].rec.Anchor
		if a != c {
			return a[0] < c[0] || a[0] == c[0] && a[1] < c[1]
		}
		return entries[i].pre < entries[j].pre
	})
	sum := sha256.Sum256(src)
	header := Header{Task: task, Len: len(src), SHA256: hex.EncodeToString(sum[:])}
	ledger := &Ledger{Task: task, Source: LedgerSource{ID: id, Bytes: len(src), SHA256: header.SHA256},
		Certificate: cert.Summary(), Records: []Record{}}
	kinds := envelopeKinds[task]
	var streams []Stream
	for k, e := range entries {
		r := e.rec
		r.ID = fmt.Sprintf("%s:%s:%d", task, id, k)
		ledger.Records = append(ledger.Records, r)
		if r.Cand == nil {
			continue
		}
		header.Cand = r.ID
		env, cand := *r.Envelope, *r.Cand
		streams = append(streams, Stream{Header: header, Events: []Event{
			{E: "OPEN", K: kinds[0], S: env[0], T: env[1]},
			{E: "LEAF", K: kinds[1], S: cand[0], T: cand[1]},
			{E: "CAND", ID: r.ID, S: cand[0], T: cand[1]},
			{E: "CLOSE", K: kinds[0]},
		}})
	}
	return streams, ledger, nil
}

func (b *builder) p3(file *ast.File) ([]entry, error) {
	decls, err := b.declaring(file)
	if err != nil {
		return nil, err
	}
	out := make([]entry, 0, len(decls))
	for i, d := range decls {
		r := Record{Anchor: d.anchor, Status: "excluded_receiver_argument"}
		if d.id != nil {
			r.Name, r.Subtype = d.id.Name, d.subtype
			switch d.id.Name {
			case "_":
				r.Status = "excluded_blank"
			case ".":
				r.Status = "excluded_dot_import"
			default:
				r.Status = "candidate"
				cand, env := d.anchor, d.env
				r.Cand, r.Envelope = &cand, &env
			}
		}
		r.Reasons = []string{r.Status}
		out = append(out, entry{rec: r, pre: i})
	}
	return out, nil
}

// importPathKey returns the path that an import names: the unquoted literal, or its raw text when it does not unquote.
// This matches the importKey semantics of the analyzer.
func importPathKey(sp *ast.ImportSpec) string {
	if sp.Path == nil {
		return ""
	}
	if path, err := strconv.Unquote(sp.Path.Value); err == nil {
		return path
	}
	return sp.Path.Value
}

func (b *builder) p2(file *ast.File, cert *pkgsnap.Certificate) ([]entry, error) {
	decls, err := b.declaring(file)
	if err != nil {
		return nil, err
	}
	first := map[string]*ast.ImportSpec{}
	importNames := map[*ast.Ident]bool{}
	for _, sp := range file.Imports {
		if key := importPathKey(sp); first[key] == nil {
			first[key] = sp
		}
		if sp.Name != nil {
			importNames[sp.Name] = true
		}
	}
	// Import aliases are checked separately; exclude their declaration nodes here.
	fileNames := map[string][][2]int{}
	for _, d := range decls {
		if d.id != nil && !importNames[d.id] {
			fileNames[d.id.Name] = append(fileNames[d.id.Name], d.anchor)
		}
	}
	for _, spans := range fileNames {
		sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	}

	var out []entry
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		if !ok {
			return true
		}
		base, parenBase := "", false
		switch x := sel.X.(type) {
		case *ast.Ident:
			base = x.Name
		case *ast.ParenExpr:
			if id, ok := ast.Unparen(x).(*ast.Ident); ok {
				base, parenBase = id.Name, true
			}
		}
		if want, ok := p2Callees[base]; !ok || sel.Sel.Name != want {
			return true
		}
		r := Record{Base: base, Anchor: b.span(call)}
		var lit *ast.BasicLit
		if len(call.Args) > 0 {
			r.Anchor = b.span(call.Args[0])
			if l, ok := call.Args[0].(*ast.BasicLit); ok && l.Kind == token.STRING {
				lit = l
			}
		}
		reason := func(ok bool, status string) {
			if ok {
				r.Reasons = append(r.Reasons, status)
			}
		}
		reason(parenBase, "excluded_paren_base")
		reason(len(call.Args) == 0, "no_argument")
		reason(len(call.Args) > 0 && lit == nil, "indirect_argument")
		if lit != nil {
			_, err := strconv.Unquote(lit.Value)
			reason(err != nil, "undecodable")
		}
		sp := first[base]
		if sp != nil {
			imp := b.span(sp)
			r.Import = &imp
		}
		reason(sp == nil || sp.Name != nil, "excluded_import")
		r.FileConflicts = fileNames[base]
		reason(len(r.FileConflicts) > 0, "excluded_file_conflict")
		conflicts, complete := cert.Evidence(base)
		r.PackageConflicts = conflicts
		reason(len(conflicts) > 0, "excluded_package_conflict")
		reason(!complete && len(r.FileConflicts) == 0 && len(conflicts) == 0, "unknown_context")
		reason(len(r.Reasons) == 0, "eligible")
		r.Status = r.Reasons[0]
		if !parenBase && lit != nil {
			cand, env := b.span(lit), [2]int{b.off(call.Pos()), b.off(lit.End())}
			r.Cand, r.Envelope = &cand, &env
		}
		out = append(out, entry{rec: r, pre: len(out)})
		return true
	})
	return out, nil
}
