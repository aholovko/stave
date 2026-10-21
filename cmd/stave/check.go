package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/aholovko/stave/internal/diag"
	"github.com/aholovko/stave/internal/pkgsnap"
)

// group selects a whole package or named files with their siblings retained as context.
type group struct {
	dir  string   // as first given, so results read the way the request did
	key  string   // the absolute path of the directory, its identity across spellings
	only []string // base names; nil means every target
}

func runCheck(args []string, stdout, stderr io.Writer) int {
	var o options
	fs := flagSet("check", stderr)
	asJSON := fs.Bool("json", false, "print one stave-report-v1 JSON object per package")
	o.buildFlags(fs)
	o.advisoryFlags(fs)
	if code := parse(fs, args); code >= 0 {
		return code
	}
	o.visited(fs)
	if _, err := o.config(); err != nil {
		fmt.Fprintf(stderr, "stave check: %v\n", err)
		return exitUsage
	}
	if fs.NArg() == 0 {
		fmt.Fprintf(stderr, "stave check: no package directory or Go file named\n%s", usage)
		return exitUsage
	}
	data, self, err := o.installation()
	if err != nil {
		fmt.Fprintf(stderr, "stave check: %v\n", err)
		return exitFailure
	}
	cfg := o.configure(data, self)()

	code := exitOK
	groups, err := groupPaths(fs.Args())
	if err != nil {
		fmt.Fprintf(stderr, "stave check: %v\n", err)
		return exitFailure
	}
	var (
		shown []group
		snaps []*pkgsnap.Snapshot
		reps  []diag.PackageReport
	)
	for _, g := range groups {
		snap, rep, err := cfg.Exact(g.dir, g.only)
		if err != nil {
			fmt.Fprintf(stderr, "stave check: %v\n", err)
			code = exitFailure
			continue
		}
		shown, snaps, reps = append(shown, g), append(snaps, snap), append(reps, rep)
	}
	// On Unix, the backend has its own process group, so forward interrupts through the context. Keep signals
	// intercepted until Advise returns to prevent a second interrupt from leaving child processes behind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reps = cfg.Advise(ctx, snaps, reps)

	for i, rep := range reps {
		if *asJSON {
			if err := json.NewEncoder(stdout).Encode(rep); err != nil {
				fmt.Fprintf(stderr, "stave check: %v\n", err)
				return exitFailure
			}
		} else {
			writeText(stdout, shown[i], snaps[i], rep)
		}
	}
	if len(reps) > 0 && reps[0].Advisory.Status == diag.StatusError {
		fmt.Fprintf(stderr, "stave check: advisory provider %s failed: %s\n", reps[0].Advisory.Provider, reps[0].Advisory.Reason)
		code = exitFailure
	}
	// Class errors fail the command; unsupported coverage is a stated limit. Avoid repeating a whole-exchange error
	// for every class.
	for i, rep := range reps {
		for _, f := range rep.Files {
			if f.Role != diag.RoleTarget && f.Role != diag.RoleTestTarget {
				continue
			}
			for _, c := range f.Coverage {
				if c.Status != diag.StatusError {
					continue
				}
				if c.Origin == diag.OriginAdvisory && rep.Advisory.Status == diag.StatusError {
					continue
				}
				fmt.Fprintf(stderr, "stave check: %s: %s %s error: %s\n", filepath.Join(shown[i].dir, f.Name), c.Class, c.Origin, c.Reason)
				code = exitFailure
			}
		}
	}
	return code
}

// Canonicalize package paths so alternate spellings do not duplicate analysis or backend inputs.
// Naming a directory selects all targets, even when individual files were also named.
func groupPaths(paths []string) ([]group, error) {
	var groups []group
	whole := map[string]bool{}
	index := map[string]int{}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		dir, name := filepath.Clean(p), ""
		if !info.IsDir() {
			dir, name = filepath.Split(dir)
			dir = filepath.Clean(dir)
		}
		key, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		i, seen := index[key]
		if !seen {
			i = len(groups)
			index[key] = i
			groups = append(groups, group{dir: dir, key: key})
		}
		switch {
		case name == "":
			whole[key] = true
		case !slices.Contains(groups[i].only, name):
			groups[i].only = append(groups[i].only, name)
		}
	}
	for i := range groups {
		if whole[groups[i].key] {
			groups[i].only = nil
		}
	}
	return groups, nil
}

// Text positions use byte columns. Coverage and candidate counts remain separate from findings.
func writeText(w io.Writer, g group, snap *pkgsnap.Snapshot, rep diag.PackageReport) {
	for _, f := range rep.Files {
		path := filepath.Join(g.dir, f.Name)
		if f.Role != diag.RoleTarget && f.Role != diag.RoleTestTarget {
			if f.Reason != "not requested" {
				fmt.Fprintf(w, "%s: not analysed (%s): %s\n", path, f.Role, f.Reason)
			}
			continue
		}
		file, _ := snap.File(f.Name)
		for _, d := range f.Diagnostics {
			line, col := diag.LineCol(file.Src, d.Span.Start)
			fmt.Fprintf(w, "%s:%d:%d: %s %s: %s%s\n", path, line, col, d.Class, d.Origin, d.Message, provenance(d))
		}

		// Exact and learned classes share labels, so coverage must name the origin.
		summary := map[string][]string{}
		for _, c := range f.Coverage {
			summary[c.Origin] = append(summary[c.Origin], c.Class+"="+c.Status)
		}
		fmt.Fprintf(w, "%s: coverage: %s %s; %s %s\n", path,
			diag.OriginExact, strings.Join(summary[diag.OriginExact], " "),
			diag.OriginAdvisory, strings.Join(summary[diag.OriginAdvisory], " "))
		for _, c := range f.Coverage {
			if line := diag.Candidates(c, advised(f, c.Class)); line != "" {
				fmt.Fprintf(w, "%s: %s\n", path, line)
			}
		}
		for _, line := range diag.NotJudged(f.Coverage) {
			fmt.Fprintf(w, "%s: not judged: %s\n", path, line)
		}
	}
}

func advised(f diag.FileReport, class string) int {
	n := 0
	for _, d := range f.Diagnostics {
		if d.Origin == diag.OriginAdvisory && d.Class == class {
			n++
		}
	}
	return n
}

// Scores are raw backend values, not probabilities.
func provenance(d diag.Diagnostic) string {
	if d.Origin != diag.OriginAdvisory || d.Backend == nil {
		return ""
	}
	if d.Score == nil {
		return fmt.Sprintf(" [%s]", d.Backend.ID)
	}
	return fmt.Sprintf(" [%s score=%.2f]", d.Backend.ID, *d.Score)
}
