// Package harness combines snapshots, exact analysis and advisory predictions
// so the CLI and language server report the same findings and coverage.
package harness

import (
	"fmt"
	"runtime"
	"sort"

	"github.com/aholovko/stave/internal/advisory"
	"github.com/aholovko/stave/internal/analyzer"
	"github.com/aholovko/stave/internal/diag"
	"github.com/aholovko/stave/internal/instance"
	"github.com/aholovko/stave/internal/pkgsnap"
)

var classNames = map[string]string{
	"P1": "else after direct return",
	"P2": "error-literal form",
	"P3": "underscore at a declaring position",
	"TF": "missing diagnostic details in test failures",
	"DC": "documentation comments",
	"RP": "result-parameter clarity",
}

// contextClasses are the classes judged only under a complete package context.
var contextClasses = map[string]bool{"P2": true}

// exactBackend identifies the exact analyzer. The toolchain is its detail, because the parser and the build-constraint
// rules come from it.
var exactBackend = &diag.Backend{ID: "stave-exact", Detail: runtime.Version()}

const notRun = "advisory analysis has not run"

const noProvider = "no advisory provider is configured"

// Config selects package analysis. Nil Provider disables advice; Disabled explains
// why. Key identifies the provider configuration so presenters can distinguish results.
type Config struct {
	Build    pkgsnap.Build
	Provider advisory.Provider
	Disabled string
	Key      string
}

// Exact snapshots dir and runs the exact analyzer over its targets. It fails only when no snapshot exists. Everything
// that the analysis itself cannot do is stated in the coverage of the report.
func (c Config) Exact(dir string, only []string) (*pkgsnap.Snapshot, diag.PackageReport, error) {
	snap, err := pkgsnap.Take(dir, c.Build, only)
	if err != nil {
		return nil, diag.PackageReport{}, err
	}
	return snap, Report(snap), nil
}

// Report returns the exact analysis of one snapshot. It reads only the snapshot, so the report binds exactly the bytes
// and the context it names.
func Report(snap *pkgsnap.Snapshot) diag.PackageReport {
	rep := diag.PackageReport{
		Schema: diag.Schema,
		Dir:    snap.Dir,
		Build: diag.Build{
			GOOS: snap.Build.GOOS, GOARCH: snap.Build.GOARCH,
			Tags: append([]string{}, snap.Build.Tags...), Cgo: snap.Build.Cgo,
			Go: runtime.Version(),
		},
		Advisory: diag.Advisory{
			Provider: "disabled", Status: diag.StatusDisabled, Reason: notRun, Classes: []string{},
		},
		Context: diag.Context{
			Digest:     snap.Context.Digest(),
			Complete:   snap.Context.Complete(),
			Incomplete: append([]string{}, snap.Incomplete...),
		},
	}
	for _, f := range snap.Files {
		fr := diag.FileReport{
			Name: f.Name, Path: f.Path, SHA256: f.SHA256, Package: f.Package,
			Role: string(f.Role), Reason: f.Reason,
			Coverage: []diag.Coverage{}, Diagnostics: []diag.Diagnostic{},
		}
		switch f.Role {
		case pkgsnap.RoleTarget:
			fr.Diagnostics, fr.Coverage = exactFile(snap, f)
		case pkgsnap.RoleTestTarget:
			fr.Coverage = testFile()
		}
		rep.Files = append(rep.Files, fr)
	}
	return rep
}

// Exact analysis requires package evidence, which excludes test files.
func testFile() []diag.Coverage {
	var coverage []diag.Coverage
	for _, class := range diag.ExactClasses {
		coverage = append(coverage, diag.Coverage{
			Class: class, Origin: diag.OriginExact, Status: diag.StatusUnsupported,
			Reason: pkgsnap.TestsOutsideEvidence, Items: []diag.CoverageItem{},
		})
	}
	return append(coverage, advisoryCoverage()...)
}

func advisoryCoverage() []diag.Coverage {
	var coverage []diag.Coverage
	for _, class := range diag.AdvisoryClasses {
		coverage = append(coverage, diag.Coverage{
			Class: class, Origin: diag.OriginAdvisory, Status: diag.StatusDisabled,
			Reason: notRun, Items: []diag.CoverageItem{},
		})
	}
	return coverage
}

// exactFile analyses one target. Every way the analysis can fail becomes error coverage for the classes it affects.
// The other classes of the file and the other files of the package are still reported.
func exactFile(snap *pkgsnap.Snapshot, f pkgsnap.File) ([]diag.Diagnostic, []diag.Coverage) {
	findings, failed := analyse(snap, f)

	diagnostics := []diag.Diagnostic{}
	for _, class := range diag.ExactClasses {
		for _, s := range findings[class] {
			diagnostics = append(diagnostics, diag.Diagnostic{
				Class:         class,
				Origin:        diag.OriginExact,
				Path:          f.Path,
				SourceSHA256:  f.SHA256,
				ContextDigest: snap.Context.Digest(),
				Span:          s,
				Message:       classNames[class],
				Backend:       exactBackend,
			})
		}
	}
	sort.SliceStable(diagnostics, func(i, j int) bool { return diagnostics[i].Span.Start < diagnostics[j].Span.Start })

	var coverage []diag.Coverage
	for _, class := range diag.ExactClasses {
		c := diag.Coverage{Class: class, Origin: diag.OriginExact, Status: diag.StatusChecked, Items: []diag.CoverageItem{}}
		switch {
		case failed[class] != "":
			c.Status, c.Reason = diag.StatusError, failed[class]
		case contextClasses[class] && !snap.Context.Complete():
			// Incomplete context suppresses P2 analysis, so no finding does not mean a negative.
			c.Status = diag.StatusUnsupported
			c.Reason = "incomplete package context: a sibling's declarations are unknowable"
			for _, sibling := range snap.Incomplete {
				c.Items = append(c.Items, diag.CoverageItem{Status: diag.StatusUnsupported, Reason: sibling})
			}
		}
		coverage = append(coverage, c)
	}
	return diagnostics, append(coverage, advisoryCoverage()...)
}

// analyse runs the exact analyzer over one target and guards its spans. A failure to build the instance or to analyse
// it fails every exact class.
func analyse(snap *pkgsnap.Snapshot, f pkgsnap.File) (map[string][]diag.Span, map[string]string) {
	inst, err := bind(snap, f)
	if err != nil {
		return failAll(err)
	}
	return judge(inst)
}

// bind builds the analysis instance of one snapshot file. It succeeds only when the bytes still hash to the
// manifest row of the file.
func bind(snap *pkgsnap.Snapshot, f pkgsnap.File) (*instance.Instance, error) {
	return instance.New(f.Name, snap.Context, f.Src)
}

func judge(inst *instance.Instance) (map[string][]diag.Span, map[string]string) {
	occurrences, err := analyzer.Analyze(inst)
	if err != nil {
		return failAll(err)
	}
	return adapt(inst.Source(), occurrences)
}

func failAll(err error) (map[string][]diag.Span, map[string]string) {
	failed := map[string]string{}
	for _, class := range diag.ExactClasses {
		failed[class] = fmt.Sprintf("exact analysis failed: %v", err)
	}
	return nil, failed
}
