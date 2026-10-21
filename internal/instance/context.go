// Package instance builds analysis inputs from checked source bytes and package context.
package instance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Row identifies a sibling by its content hash, its exclusion status, or both. Readable but excluded files still
// contribute declarations that can shadow names. Unread files have no hash or declarations, so unknown content
// cannot be mistaken for known content.
type Row struct {
	Path   string
	SHA    string // lowercase hex SHA-256; empty when source bytes are unavailable
	Status string // empty exactly when the file is eligible for analysis

	// TopDecls supplies package-block names for shadowing checks. It requires a SHA because declarations are
	// known only for files that were read, regardless of their exclusion status.
	TopDecls []string
}

func (r Row) valid() error {
	if r.Path == "" {
		return fmt.Errorf("row: empty path")
	}
	if strings.ContainsRune(r.Path, 0) {
		return fmt.Errorf("row %q: path contains NUL, which would break digest injectivity", r.Path)
	}
	if r.SHA == "" && r.Status == "" {
		return fmt.Errorf("row %q: at least one of SHA and Status must be set", r.Path)
	}
	if r.SHA != "" {
		if len(r.SHA) != 64 {
			return fmt.Errorf("row %q: SHA is %d hex characters, want 64", r.Path, len(r.SHA))
		}
		if _, err := hex.DecodeString(r.SHA); err != nil {
			return fmt.Errorf("row %q: SHA is not lowercase hex: %w", r.Path, err)
		}
		if r.SHA != strings.ToLower(r.SHA) {
			return fmt.Errorf("row %q: SHA must be lowercase hex", r.Path)
		}
	} else if len(r.TopDecls) != 0 {
		return fmt.Errorf(
			"row %q: declarations without a hash; a file whose bytes were never read declares nothing knowable",
			r.Path)
	}
	if r.Status != "" && strings.ContainsAny(r.Status, "\x00\n") {
		return fmt.Errorf("row %q: status contains NUL or LF, which would break digest injectivity", r.Path)
	}
	return nil
}

// PackageContext includes non-test Go siblings, even those excluded from analysis: their declarations can still
// shadow names.
type PackageContext struct {
	rows     []Row // ascending Path order, deduplicated
	complete bool
	digest   string
	declared map[string]bool
}

// NewPackageContext copies and validates the rows. Complete must be false when sibling declarations are unknown,
// so callers cannot certify that a name is unshadowed.
func NewPackageContext(rows []Row, complete bool) (*PackageContext, error) {
	sorted := make([]Row, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	declared := map[string]bool{}
	for i, r := range sorted {
		if err := r.valid(); err != nil {
			return nil, err
		}
		if i > 0 && sorted[i-1].Path == r.Path {
			return nil, fmt.Errorf("row %q: duplicate path", r.Path)
		}
		for _, name := range r.TopDecls {
			declared[name] = true
		}
	}
	pc := &PackageContext{rows: sorted, complete: complete, declared: declared}
	pc.digest = pc.computeDigest()
	return pc, nil
}

// computeDigest writes rows in path order as path, NUL, hash-or-STATUS, LF. A known hash takes precedence so
// changing an exclusion status alone does not change file identity. NUL separates paths from values because paths
// may contain newlines; Row.valid excludes NUL from paths and delimiters from values.
func (pc *PackageContext) computeDigest() string {
	h := sha256.New()
	for _, r := range pc.rows {
		h.Write([]byte(r.Path))
		h.Write([]byte{0})
		if r.SHA != "" {
			h.Write([]byte(r.SHA))
		} else {
			h.Write([]byte("STATUS:" + r.Status))
		}
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Digest identifies the active package file set.
func (pc *PackageContext) Digest() string { return pc.digest }

// Complete reports whether all in-scope sibling declarations are known.
func (pc *PackageContext) Complete() bool { return pc.complete }

// Rows returns the manifest rows in ascending path order.
func (pc *PackageContext) Rows() []Row {
	out := make([]Row, len(pc.rows))
	copy(out, pc.rows)
	return out
}

// Declares reports whether any package file declares name at package block scope.
func (pc *PackageContext) Declares(name string) bool { return pc.declared[name] }

// PkgFlagNames defines the order of the package-flag vector. Consumers depend on this fixed order and length.
var PkgFlagNames = []string{
	"context_complete",
	"toplevel_conflict_errors",
	"toplevel_conflict_fmt",
	"shadowed_int",
	"shadowed_float64",
	"shadowed_string",
	"shadowed_rune",
	"shadowed_complex128",
}

// shadowedTypes lists the predeclared default types in package-flag order.
var shadowedTypes = []string{"int", "float64", "string", "rune", "complex128"}

// pkgFlags records context completeness and package-scope shadowing in package-flag order.
func (pc *PackageContext) pkgFlags() []bool {
	out := make([]bool, 0, len(PkgFlagNames))
	out = append(out, pc.complete)
	out = append(out, pc.Declares("errors"), pc.Declares("fmt"))
	for _, name := range shadowedTypes {
		out = append(out, pc.Declares(name))
	}
	return out
}
