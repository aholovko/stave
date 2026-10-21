// Package pkgsnap captures package files once so findings, source hashes and context
// all describe the same bytes.
//
// Evidence includes every regular non-test Go file, even generated or build-excluded
// files, because their declarations can shadow names. Targets are the files selected
// for analysis; narrowing them must not narrow the evidence.
package pkgsnap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/aholovko/stave/internal/astinfo"
	"github.com/aholovko/stave/internal/instance"
)

// MaxFileBytes bounds snapshot memory use. Larger siblings make the context incomplete
// rather than disappearing from the evidence.
const MaxFileBytes = 8 << 20

// Build selects analysis targets without changing package evidence or its digest.
// Architecture feature tags follow GO$GOARCH or the toolchain default. TestTargets
// includes test files for analysis, but they never contribute package evidence.
type Build struct {
	GOOS        string
	GOARCH      string
	Tags        []string
	Cgo         bool
	TestTargets bool
}

// Role says what a file contributes to one snapshot.
type Role string

const (
	// RoleTarget files are analysed and reported on.
	RoleTarget Role = "target"
	// RoleTestTarget files are test files analysed on request. They are never evidence.
	RoleTestTarget Role = "test-target"
	// RoleContextOnly files contribute evidence but are not analysed.
	RoleContextOnly Role = "context-only"
	// RoleIgnored files were requested but lie outside the retained evidence, or are test files the selection cannot
	// analyse.
	RoleIgnored Role = "ignored"
)

// TestsOutsideEvidence is why a test file is never evidence, nor judged by an analysis bound to the evidence.
const TestsOutsideEvidence = "test files lie outside the retained evidence"

// The evidence statuses. A status other than StatusOK means the file's declarations are unknowable. This makes the
// package context incomplete.
const (
	StatusOK         = ""
	StatusParseError = "parse-error"
	StatusOversized  = "oversized"
	StatusUnreadable = "unreadable"
)

// File is one directory entry of the snapshot, bound to the bytes that were read for it.
type File struct {
	Name    string // base name: the manifest row path
	Path    string // Dir joined with Name
	Status  string // StatusOK, or why the declarations are unknowable
	SHA256  string // lowercase hex of Src; empty exactly when the bytes were not read
	Package string // package clause name
	// PackageSpan is the byte range of the name in the package clause. A presenter anchors a statement about the whole
	// file there.
	PackageSpan [2]int
	Generated   bool // ast.IsGenerated
	ImportsC    bool // imports "C", so it builds only with cgo enabled
	Role        Role
	Reason      string // why the file is not a target; empty for a target
	Src         []byte // nil exactly when the bytes were not read
}

// Snapshot is one read of a package directory.
type Snapshot struct {
	Dir     string // absolute
	Build   Build
	Files   []File // ascending Name order
	Context *instance.PackageContext

	// Incomplete names every sibling whose declarations are unknowable, with its status. It is empty exactly when
	// Context.Complete() holds.
	Incomplete []string
}

// File returns the snapshot entry with the given base name.
func (s *Snapshot) File(name string) (File, bool) {
	for _, f := range s.Files {
		if f.Name == name {
			return f, true
		}
	}
	return File{}, false
}

// Take reads dir once and returns its snapshot. A non-empty only narrows the targets to those base names. It never
// narrows the evidence.
func Take(dir string, b Build, only []string) (*Snapshot, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", dir, err)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", dir, err)
	}
	if b.GOOS == "" {
		b.GOOS = build.Default.GOOS
	}
	if b.GOARCH == "" {
		b.GOARCH = build.Default.GOARCH
	}
	tags, err := toolTags(b.GOARCH)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", dir, err)
	}

	snap := &Snapshot{Dir: abs, Build: b}
	var rows []instance.Row
	for _, entry := range entries { // os.ReadDir sorts by name
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, row, ok := readEvidence(abs, name)
		if !ok {
			continue
		}
		if f.Status != StatusOK {
			snap.Incomplete = append(snap.Incomplete, name+": "+f.Status)
		}
		rows = append(rows, row)
		snap.Files = append(snap.Files, f)
	}

	// NewPackageContext trusts the completeness flag, so derive it from the same
	// reads as the evidence rows.
	snap.Context, err = instance.NewPackageContext(rows, len(snap.Incomplete) == 0)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", dir, err)
	}
	if b.TestTargets {
		snap.readTests(entries, only)
	}
	snap.selectTargets(only, tags)
	if err := snap.recordIgnored(only); err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", dir, err)
	}
	sort.Slice(snap.Files, func(i, j int) bool { return snap.Files[i].Name < snap.Files[j].Name })
	return snap, nil
}

// Read tests after building the context so they cannot contribute evidence.
func (s *Snapshot) readTests(entries []os.DirEntry, only []string) {
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, "_test.go") || (len(only) > 0 && !slices.Contains(only, name)) {
			continue
		}
		if f, _, ok := readEvidence(s.Dir, name); ok {
			s.Files = append(s.Files, f)
		}
	}
}

// Keep unsupported requested files visible in the report instead of silently omitting them.
func (s *Snapshot) recordIgnored(only []string) error {
	for _, name := range only {
		if name != filepath.Base(name) {
			return fmt.Errorf("%q is not a base name within the package directory", name)
		}
		if _, ok := s.File(name); ok {
			continue
		}
		f := File{Name: name, Path: filepath.Join(s.Dir, name), Role: RoleIgnored}
		info, err := os.Stat(f.Path)
		switch {
		case err != nil:
			return err
		case !info.Mode().IsRegular():
			f.Reason = "not a regular file"
		case strings.HasSuffix(name, "_test.go"):
			f.Reason = TestsOutsideEvidence
		default:
			f.Reason = "not a Go source file"
		}
		s.Files = append(s.Files, f)
	}
	return nil
}

// readEvidence reads one candidate sibling and derives its manifest row from the bytes it read. It follows symlinks. It
// reports ok=false for an entry that resolves to something other than a regular file. Such an entry is never opened.
func readEvidence(dir, name string) (File, instance.Row, bool) {
	f := File{Name: name, Path: filepath.Join(dir, name)}
	unknowable := func(status, detail string) (File, instance.Row, bool) {
		f.Status, f.Reason = status, status+": "+detail
		return f, instance.Row{Path: name, SHA: f.SHA256, Status: status}, true
	}

	info, err := os.Stat(f.Path)
	if err != nil {
		return unknowable(StatusUnreadable, err.Error())
	}
	if !info.Mode().IsRegular() {
		return File{}, instance.Row{}, false
	}
	if info.Size() > MaxFileBytes {
		return unknowable(StatusOversized, fmt.Sprintf("%d bytes exceeds the %d byte limit", info.Size(), MaxFileBytes))
	}
	src, err := readBounded(f.Path)
	if err != nil {
		return unknowable(StatusUnreadable, err.Error())
	}
	if len(src) > MaxFileBytes { // grew between the stat and the read
		return unknowable(StatusOversized, fmt.Sprintf("more than the %d byte limit", MaxFileBytes))
	}
	sum := sha256.Sum256(src)
	f.Src, f.SHA256 = src, hex.EncodeToString(sum[:])

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, name, src, astinfo.ParserMode)
	if err != nil {
		return unknowable(StatusParseError, err.Error())
	}
	decls := astinfo.TopDeclNames(parsed)
	f.Package = parsed.Name.Name
	f.PackageSpan = [2]int{fset.Position(parsed.Name.Pos()).Offset, fset.Position(parsed.Name.End()).Offset}
	f.Generated = ast.IsGenerated(parsed)
	for _, imp := range parsed.Imports {
		// Raw and escaped spellings of "C" must obey the same cgo rule.
		if imp.Path != nil {
			if path, err := strconv.Unquote(imp.Path.Value); err == nil && path == "C" {
				f.ImportsC = true
			}
		}
	}
	return f, instance.Row{Path: name, SHA: f.SHA256, TopDecls: decls}, true
}

func readBounded(path string) ([]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	return io.ReadAll(io.LimitReader(fh, MaxFileBytes+1))
}

// selectTargets uses captured bytes so target selection and evidence cannot refer
// to different file revisions. toolTags supplies the selected architecture's tags;
// MatchFile does not derive them from GOARCH.
func (s *Snapshot) selectTargets(only []string, toolTags []string) {
	requested := map[string]bool{}
	for _, name := range only {
		requested[name] = true
	}

	ctxt := build.Default
	ctxt.GOOS, ctxt.GOARCH = s.Build.GOOS, s.Build.GOARCH
	ctxt.ToolTags = toolTags
	ctxt.BuildTags = append([]string(nil), s.Build.Tags...)
	ctxt.CgoEnabled = s.Build.Cgo
	ctxt.UseAllFiles = false
	ctxt.OpenFile = func(path string) (io.ReadCloser, error) {
		for _, f := range s.Files {
			if f.Path == path && f.Src != nil {
				return io.NopCloser(bytes.NewReader(f.Src)), nil
			}
		}
		return nil, fmt.Errorf("%s is not in the snapshot", path)
	}

	for i := range s.Files {
		f := &s.Files[i]
		// Test files can be targets, but never package evidence.
		test := strings.HasSuffix(f.Name, "_test.go")
		f.Role = RoleContextOnly
		if test {
			f.Role = RoleIgnored
		}
		if f.Status != StatusOK {
			continue // readEvidence already recorded the reason
		}
		if len(only) > 0 && !requested[f.Name] {
			f.Reason = "not requested"
			continue
		}
		switch match, err := ctxt.MatchFile(s.Dir, f.Name); {
		case err != nil:
			f.Reason = fmt.Sprintf("build constraints could not be evaluated: %v", err)
		case !match:
			f.Reason = fmt.Sprintf("excluded by the build selection %s/%s", s.Build.GOOS, s.Build.GOARCH)
		case f.ImportsC && !s.Build.Cgo:
			// MatchFile stops short of the cgo rule that build.Import applies: a file importing "C" is not built when
			// cgo is disabled.
			f.Reason = "imports \"C\" while the build selection disables cgo"
		case f.Generated:
			f.Reason = "generated file"
		case test:
			f.Role = RoleTestTarget
		default:
			f.Role = RoleTarget
		}
	}
}
