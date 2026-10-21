package structural

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"strings"

	"github.com/aholovko/stave/internal/astinfo"
	"github.com/aholovko/stave/internal/tfcand"
	"github.com/aholovko/stave/internal/tfsentry"
)

// TFLedgerSchema names the TF occurrence ledger document.
const TFLedgerSchema = "stave-tf-ledger-1"

// TFLedger keeps TF coverage and declaration context alongside each occurrence.
// The independent sentry exposes missed selectors. Paths, reasons and statuses
// stay out of event streams and model features.
type TFLedger struct {
	Schema string          `json:"schema"`
	Source LedgerSource    `json:"source"`
	Path   string          `json:"path"`
	Gate   TFGate          `json:"path_gate"`
	File   *tfcand.FileRow `json:"file"`
	Rows   []TFRow         `json:"rows"`
	Sentry TFSentry        `json:"sentry"`
}

// TFGate is eligible, or excluded with not_test_file, vendor or testdata.
type TFGate struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// TFRow is one original row. Cand and Context are set exactly when the row is admitted. A context that cannot be
// derived stays attribute_underivable and is never a negative.
type TFRow struct {
	tfcand.CallRow
	Cand    string                `json:"cand,omitempty"`
	Context *tfcand.ContextResult `json:"context,omitempty"`
}

// TFSentry reconciles each scanner hit with an occurrence row. Unmatched hits
// remain unknown coverage, never exclusions or checked negatives.
type TFSentry struct {
	Hits       []TFHit  `json:"hits"`
	Total      int      `json:"total"`
	Matched    int      `json:"matched"`
	Unresolved int      `json:"unresolved"`
	Errors     []string `json:"errors"`
}

// TFHit is one sentry hit and its reconciliation.
type TFHit struct {
	tfsentry.Hit
	Row    *int   `json:"row"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// BuildTFLedger returns the TF ledger and one stream per admitted row, in row order.
// It verifies src against sha and checks path text without opening the path.
// Identity or inconsistent enumeration is an error; unsupported files and unmatched
// sentry hits remain visible in the ledger.
func BuildTFLedger(id, path, sha string, src []byte) ([]Stream, *TFLedger, error) {
	if len(sha) != sha256.Size*2 || strings.ToLower(sha) != sha {
		return nil, nil, errors.New("expected SHA-256 must be 64 lowercase hex digits")
	}
	sum := sha256.Sum256(src)
	if got := hex.EncodeToString(sum[:]); got != sha {
		return nil, nil, fmt.Errorf("source SHA-256 %s does not match the expected %s", got, sha)
	}
	if path == "" {
		return nil, nil, errors.New("TF ledger needs the source path")
	}
	ledger := &TFLedger{Schema: TFLedgerSchema, Source: LedgerSource{ID: id, Bytes: len(src), SHA256: sha},
		Path: path, Gate: gate(path), Rows: []TFRow{}}
	scan := tfsentry.Scan(src)
	if ledger.Gate.Status != "eligible" {
		sentry, err := reconcile("path_excluded", nil, scan)
		ledger.Sentry = sentry
		return nil, ledger, err
	}
	p := tfcand.Screen(path, src)
	ledger.File = &p.Row
	sentry, err := reconcile(p.Row.Status, p.Rows, scan)
	if err != nil {
		return nil, nil, err
	}
	ledger.Sentry = sentry
	for _, r := range p.Rows {
		ledger.Rows = append(ledger.Rows, TFRow{CallRow: r})
	}
	if len(p.Admitted) != p.Row.Admitted {
		return nil, nil, fmt.Errorf("%d admitted occurrences for %d admitted rows", len(p.Admitted), p.Row.Admitted)
	}
	b := &builder{src: src, fset: p.Fset}
	header := Header{Task: "TF", Len: len(src), SHA256: sha}
	streams := make([]Stream, 0, len(p.Admitted))
	for k, o := range p.Admitted {
		header.Cand = fmt.Sprintf("TF:%s:%d", id, k)
		row := &ledger.Rows[o.Row]
		ctx, err := tfContext(src, &p.Row, &row.CallRow, header.Cand)
		if err != nil {
			return nil, nil, err
		}
		row.Cand, row.Context = header.Cand, &ctx
		b.events, b.candID = nil, header.Cand
		b.tfRoot(&p, o)
		streams = append(streams, Stream{Header: header, Events: b.events})
	}
	return streams, ledger, nil
}

// Interpret both path separators so Windows paths are gated consistently on every host.
func gate(path string) TFGate {
	if !strings.HasSuffix(path, "_test.go") {
		return TFGate{Status: "excluded", Reason: "not_test_file"}
	}
	for _, part := range strings.Split(strings.ReplaceAll(path, `\`, "/"), "/") {
		if part == "vendor" || part == "testdata" {
			return TFGate{Status: "excluded", Reason: part}
		}
	}
	return TFGate{Status: "eligible"}
}

// Every target-method row must match exactly one independent sentry hit. Extra
// hits remain unresolved so incomplete admission cannot look like complete coverage.
func reconcile(status string, rows []tfcand.CallRow, scan tfsentry.Result) (TFSentry, error) {
	if scan.Total != len(scan.Hits) || scan.Hits == nil || scan.Errors == nil {
		return TFSentry{}, errors.New("sentry did not enumerate a complete hit list and total")
	}
	bySel := map[int]int{}
	for i, r := range rows {
		if !tfcand.Target(r.Method) {
			continue
		}
		if _, dup := bySel[r.Sel]; dup {
			return TFSentry{}, fmt.Errorf("two rows join the selector at %d", r.Sel)
		}
		bySel[r.Sel] = i
	}
	s := TFSentry{Hits: []TFHit{}, Total: scan.Total, Errors: scan.Errors}
	joined := map[int]bool{}
	for _, h := range scan.Hits {
		hit := TFHit{Hit: h, Status: "unknown"}
		if i, ok := bySel[h.Offset]; ok {
			hit.Row, hit.Status = &i, "row"
			joined[i] = true
			s.Matched++
		} else {
			switch status {
			case "checked":
				hit.Reason = "no_row"
			case "path_excluded":
				hit.Reason = status
			default:
				hit.Reason = "file_" + status
			}
			s.Unresolved++
		}
		s.Hits = append(s.Hits, hit)
	}
	if len(joined) != len(bySel) {
		return TFSentry{}, fmt.Errorf("%d of %d target-method rows join no sentry hit", len(bySel)-len(joined), len(bySel))
	}
	return s, nil
}

func tfContext(src []byte, file *tfcand.FileRow, row *tfcand.CallRow, cand string) (tfcand.ContextResult, error) {
	d := row.Declaration.Span
	decl := src[d.Start:d.End]
	sum := sha256.Sum256(decl)
	if hex.EncodeToString(sum[:]) != d.SHA256 || row.Target.Start < d.Start || row.Target.End > d.End {
		return tfcand.ContextResult{}, fmt.Errorf("%s: declaration slice identity differs", cand)
	}
	return tfcand.DeriveContext(tfcand.ContextRequest{
		Declaration: string(decl), Target: []int{row.Target.Start - d.Start, row.Target.End - d.Start}, ID: cand,
		TestingImports: file.TestingImports, DotImport: file.DotImport,
	}), nil
}

func (b *builder) mark(kind string, s, t int) {
	b.emit(Event{E: "MARK", K: kind, S: s, T: t})
}

// ownerSpan maps a named root owner to the doc-inclusive root span. A literal owner keeps its exact span.
func (b *builder) ownerSpan(n ast.Node) (int, int) {
	if _, ok := n.(*ast.FuncDecl); ok {
		return b.root[0], b.root[1]
	}
	return b.off(n.Pos()), b.off(n.End())
}

// A root span mismatch emits BAD so the admitted row remains visible as malformed
// instead of disappearing from the results.
func (b *builder) tfRoot(p *tfcand.Parsed, o tfcand.Occurrence) {
	fd := o.Decl
	b.tf, b.runArgs, b.inserted = &o, p.RunArgs(fd), 0
	defer func() { b.tf, b.runArgs = nil, nil }()
	start := b.off(fd.Pos())
	if fd.Doc != nil {
		start = astinfo.CommentGroupSpan(b.src, b.fset, fd.Doc).Start
	}
	b.root = [2]int{start, b.off(fd.End())}
	if d := p.Rows[o.Row].Declaration.Span; d.Start != b.root[0] || d.End != b.root[1] {
		b.emit(Event{E: "BAD", Reason: "declaration span differs from the extractor"})
	}
	b.emit(Event{E: "OPEN", K: "FuncDecl", S: b.root[0], T: b.root[1]})
	if fd.Doc != nil {
		doc := astinfo.CommentGroupSpan(b.src, b.fset, fd.Doc)
		b.emit(Event{E: "LEAF", K: "Doc", S: doc.Start, T: doc.End})
	}
	kw := b.off(fd.Type.Func)
	if !bytes.HasPrefix(b.src[kw:], []byte("func")) {
		b.emit(Event{E: "BAD", Reason: "func keyword not located"})
	}
	b.emit(Event{E: "LEAF", K: "FuncKw", S: kw, T: kw + len("func")})
	b.signature(fd.Recv, fd.Name, fd.Type)
	b.region("FuncBody", fd.Body, false)
	b.stmts(fd.Body.List)
	b.close("FuncDecl")
	if b.inserted != 1 {
		b.emit(Event{E: "BAD", Reason: fmt.Sprintf("candidate inserted %d times", b.inserted)})
	}
}

func (b *builder) tfCand() {
	c := b.tf.Call
	b.emit(Event{E: "CAND", ID: b.candID, S: b.off(b.tf.Stmt.Pos()), T: b.off(b.tf.Stmt.End())})
	b.mark("CandArgs", b.off(c.Lparen)+1, b.off(c.Rparen))
	s, t := b.ownerSpan(b.tf.Owner)
	b.mark("CandBind", s, t)
	b.inserted++
}
