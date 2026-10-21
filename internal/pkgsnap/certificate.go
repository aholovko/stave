package pkgsnap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"

	"github.com/aholovko/stave/internal/astinfo"
	"github.com/aholovko/stave/internal/instance"
)

// ErrTargetIdentity means the source cannot be matched to a verified non-test target row.
var ErrTargetIdentity = errors.New("target identity mismatch")

// StatusSHAMismatch marks a row whose captured bytes no longer hash to the SHA recorded when they were read. Such a row
// is unverified and declares nothing.
const StatusSHAMismatch = "sha-mismatch"

// certBases are the package-block names that the P2 gate asks about. The summary reports their conflict rows.
var certBases = []string{"errors", "fmt"}

// Certificate binds a target to verified package evidence. Private fields ensure
// only Certify can establish that evidence. A nil or zero certificate means unknown context.
type Certificate struct {
	dir       string // absolute listing directory; empty for the zero value
	target    string
	targetSrc []byte // captured bytes of a verified target row
	rows      []evidenceRow
	complete  bool
	digest    string
}

type evidenceRow struct {
	RowSummary
	decls map[string]bool
}

// CertificateSummary is an audit record, not reusable evidence: consumers must
// recertify. FileSetDigest identifies paths and their hashes or statuses, not declarations
// or completeness.
type CertificateSummary struct {
	Listing       ListingSummary      `json:"listing"`
	Target        string              `json:"target"`
	Rows          []RowSummary        `json:"rows"`
	Complete      bool                `json:"complete"`
	FileSetDigest string              `json:"file_set_digest"`
	Conflicts     map[string][]string `json:"conflicts"`
}

// ListingSummary is the provenance of the authoritative listing.
type ListingSummary struct {
	Kind string `json:"kind"`
	Dir  string `json:"dir"`
}

// RowSummary is one evidence row. SHA256 is set only when the bytes read hash to it. On a mismatch, the recorded and
// recomputed hashes are named apart, and neither is the row's identity.
type RowSummary struct {
	Path           string   `json:"path"`
	SHA256         string   `json:"sha256,omitempty"`
	ExpectedSHA256 string   `json:"expected_sha256,omitempty"`
	ActualSHA256   string   `json:"actual_sha256,omitempty"`
	Status         string   `json:"status,omitempty"`
	Verified       bool     `json:"verified"`
	TopDecls       []string `json:"top_decls"`
}

// Certify takes the authoritative snapshot of the target's directory and certifies it. The target must be a regular
// non-test Go evidence file.
func Certify(targetPath string) (*Certificate, error) {
	abs, err := filepath.Abs(targetPath)
	if err != nil {
		return nil, fmt.Errorf("certify %s: %w", targetPath, err)
	}
	snap, err := Take(filepath.Dir(abs), Build{}, nil)
	if err != nil {
		return nil, fmt.Errorf("certify %s: %w", targetPath, err)
	}
	return certify(snap, filepath.Base(abs))
}

// Recompute declarations and completeness from captured bytes rather than trusting
// the supplied summaries. The snapshot must come from Take for its listing to be authoritative.
func certify(s *Snapshot, target string) (*Certificate, error) {
	c := &Certificate{dir: s.Dir, target: target, complete: true}
	var rows []instance.Row
	found := false
	for _, f := range s.Files {
		if f.Role == RoleIgnored || !strings.HasSuffix(f.Name, ".go") || strings.HasSuffix(f.Name, "_test.go") {
			continue
		}
		r := verifyRow(f)
		c.complete = c.complete && r.Verified
		row := instance.Row{Path: r.Path, SHA: r.SHA256, Status: r.Status, TopDecls: r.TopDecls}
		if r.Status == StatusSHAMismatch {
			row.SHA, row.TopDecls = "", nil
		}
		rows = append(rows, row)
		c.rows = append(c.rows, r)
		if f.Name == target {
			found = true
			if r.Verified {
				c.targetSrc = f.Src
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: %s is not a regular non-test Go file of %s", ErrTargetIdentity, target, s.Dir)
	}
	ctx, err := instance.NewPackageContext(rows, c.complete)
	if err != nil {
		return nil, fmt.Errorf("certify %s: %w", s.Dir, err)
	}
	c.digest = ctx.Digest()
	return c, nil
}

func verifyRow(f File) evidenceRow {
	r := evidenceRow{RowSummary: RowSummary{Path: f.Name, TopDecls: []string{}}}
	switch {
	case f.Src == nil:
		r.Status = f.Status
		if r.Status == "" {
			r.Status = StatusUnreadable
		}
		return r
	case len(f.Src) > MaxFileBytes:
		r.Status = StatusOversized
		return r
	}
	sum := sha256.Sum256(f.Src)
	actual := hex.EncodeToString(sum[:])
	if actual != f.SHA256 {
		r.Status, r.ExpectedSHA256, r.ActualSHA256 = StatusSHAMismatch, f.SHA256, actual
		return r
	}
	r.SHA256 = actual
	file, err := parser.ParseFile(token.NewFileSet(), "src.go", f.Src, astinfo.ParserMode)
	if err != nil {
		r.Status = StatusParseError
		return r
	}
	r.Verified = true
	r.decls = map[string]bool{}
	for _, name := range astinfo.TopDeclNames(file) {
		r.TopDecls = append(r.TopDecls, name)
		r.decls[name] = true
	}
	return r
}

// Verified reports whether c is a certificate Certify produced.
func (c *Certificate) Verified() bool { return c != nil && c.dir != "" }

// CheckTarget requires src to be the captured bytes of a verified target row.
func (c *Certificate) CheckTarget(src []byte) error {
	if !c.Verified() {
		return fmt.Errorf("%w: no certified target", ErrTargetIdentity)
	}
	if c.targetSrc == nil {
		return fmt.Errorf("%w: target row %s is not verified", ErrTargetIdentity, c.target)
	}
	if !bytes.Equal(c.targetSrc, src) {
		return fmt.Errorf("%w: source bytes differ from the certified %s", ErrTargetIdentity, c.target)
	}
	return nil
}

// Evidence returns the verified rows, target included, that declare base at package block, and whether every row is
// verified. A nil or zero certificate knows nothing.
func (c *Certificate) Evidence(base string) (conflicts []string, complete bool) {
	if !c.Verified() {
		return nil, false
	}
	for _, r := range c.rows {
		if r.Verified && r.decls[base] {
			conflicts = append(conflicts, r.Path)
		}
	}
	return conflicts, c.complete
}

// Summary returns the audit record, or nil for a nil or zero certificate.
func (c *Certificate) Summary() *CertificateSummary {
	if !c.Verified() {
		return nil
	}
	s := &CertificateSummary{
		Listing:       ListingSummary{Kind: "directory", Dir: c.dir},
		Target:        c.target,
		Complete:      c.complete,
		FileSetDigest: c.digest,
		Conflicts:     map[string][]string{},
	}
	for _, r := range c.rows {
		row := r.RowSummary
		row.TopDecls = append([]string{}, r.TopDecls...)
		s.Rows = append(s.Rows, row)
	}
	for _, base := range certBases {
		conflicts, _ := c.Evidence(base)
		s.Conflicts[base] = append([]string{}, conflicts...)
	}
	return s
}
