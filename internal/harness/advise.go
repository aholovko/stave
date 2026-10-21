package harness

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"unicode/utf8"

	"github.com/aholovko/stave/internal/advisory"
	"github.com/aholovko/stave/internal/diag"
	"github.com/aholovko/stave/internal/pkgsnap"
)

func targeted(role string) bool {
	return role == diag.RoleTarget || role == diag.RoleTestTarget
}

// Advise adds advisory results to copies of the exact reports. reps[i] must
// correspond to snaps[i]; exact results remain unchanged. Missing coverage is
// unsupported, and an invalid exchange is an error, so neither can appear as a
// checked negative.
func (c Config) Advise(ctx context.Context, snaps []*pkgsnap.Snapshot, reps []diag.PackageReport) []diag.PackageReport {
	out := make([]diag.PackageReport, len(reps))
	for i := range reps {
		out[i] = clone(reps[i])
	}
	if c.Provider == nil {
		reason := c.Disabled
		if reason == "" {
			reason = noProvider
		}
		return withoutAdvice(out, diag.Advisory{
			Provider: "disabled", Status: diag.StatusDisabled, Reason: reason, Classes: []string{},
		})
	}

	req := advisory.Request{Schema: advisory.Schema, Files: []advisory.FileRef{}}
	sources := map[string][]byte{}
	for _, snap := range snaps {
		for _, f := range snap.Files {
			if targeted(string(f.Role)) {
				req.Files = append(req.Files, advisory.FileRef{Path: f.Path, SHA256: f.SHA256})
				sources[f.Path] = f.Src
			}
		}
	}
	state := diag.Advisory{Provider: providerName(c.Provider), Status: diag.AdvisoryOK, Classes: []string{}}
	if len(req.Files) == 0 {
		state.Reason = "no targets to ask about"
		return withoutAdvice(out, state)
	}

	resp, err := c.Provider.Predict(ctx, req)
	var bound map[string]advisory.Bound
	if err == nil {
		bound, err = advisory.Resolve(req, resp)
	}
	if err == nil {
		err = checkSpans(bound, sources)
	}
	if err != nil {
		state.Status, state.Reason = diag.StatusError, err.Error()
		return withoutAdvice(out, state)
	}

	state.Backend = &diag.Backend{ID: resp.Backend.ID, Digest: resp.Backend.Digest, Detail: resp.Backend.Detail}
	state.Classes = append(state.Classes, resp.Classes...)
	for i := range out {
		out[i].Advisory = state
		for j := range out[i].Files {
			f := &out[i].Files[j]
			if targeted(f.Role) {
				merge(f, out[i].Context.Digest, bound[f.Path], resp.Classes, state.Backend)
			}
		}
	}
	return out
}

func providerName(p advisory.Provider) string {
	if named, ok := p.(interface{ Name() string }); ok {
		return named.Name()
	}
	return "custom"
}

// Copy the slices that advice mutates so callers can retain the exact report.
func clone(rep diag.PackageReport) diag.PackageReport {
	rep.Files = slices.Clone(rep.Files)
	for i := range rep.Files {
		rep.Files[i].Coverage = slices.Clone(rep.Files[i].Coverage)
		rep.Files[i].Diagnostics = slices.Clone(rep.Files[i].Diagnostics)
	}
	return rep
}

// withoutAdvice states one advisory outcome for every target. Each advisory class gets the same status and reason, and
// there is no advisory finding.
func withoutAdvice(reps []diag.PackageReport, state diag.Advisory) []diag.PackageReport {
	status := state.Status
	if status == diag.AdvisoryOK {
		status = diag.StatusUnsupported
	}
	for i := range reps {
		reps[i].Advisory = state
		for j := range reps[i].Files {
			f := &reps[i].Files[j]
			for k := range f.Coverage {
				if f.Coverage[k].Origin == diag.OriginAdvisory {
					f.Coverage[k].Status, f.Coverage[k].Reason = status, state.Reason
				}
			}
		}
	}
	return reps
}

// checkSpans checks every prediction against the bytes it claims to describe. A span must lie within the source and on
// rune boundaries, so every presenter can place it.
func checkSpans(bound map[string]advisory.Bound, sources map[string][]byte) error {
	for path, b := range bound {
		src := sources[path]
		for _, p := range b.Predictions {
			s := p.Span
			if s.End > len(src) {
				return fmt.Errorf("%s prediction [%d,%d) runs past the %d bytes of %s", p.Class, s.Start, s.End, len(src), path)
			}
			if !utf8.RuneStart(src[s.Start]) || (s.End < len(src) && !utf8.RuneStart(src[s.End])) {
				return fmt.Errorf("%s prediction [%d,%d) splits a rune of %s", p.Class, s.Start, s.End, path)
			}
		}
	}
	return nil
}

// Context-dependent advice must match the report's package digest. Missing context
// is unsupported; a conflicting digest is an error because it describes different evidence.
func merge(f *diag.FileReport, digest string, b advisory.Bound, served []string, backend *diag.Backend) {
	for k := range f.Coverage {
		c := &f.Coverage[k]
		if c.Origin != diag.OriginAdvisory {
			continue
		}
		c.Status, c.Reason, c.Items = diag.StatusChecked, "", []diag.CoverageItem{}
		switch {
		case !slices.Contains(served, c.Class):
			c.Status, c.Reason = diag.StatusUnsupported, fmt.Sprintf("backend %s does not serve this class", backend.ID)
		case b.Record == nil:
			c.Status, c.Reason = diag.StatusUnsupported, fmt.Sprintf("backend %s returned no record for this source", backend.ID)
		case b.Record.Status != advisory.StatusChecked:
			c.Status, c.Reason = b.Record.Status, *b.Record.Reason
		case !b.Record.Checks(c.Class):
			c.Status, c.Reason = diag.StatusUnsupported, fmt.Sprintf("backend %s did not check this class for this source", backend.ID)
		case contextClasses[c.Class] && b.Record.Context == "":
			c.Status, c.Reason = diag.StatusUnsupported, fmt.Sprintf("backend %s stated no package context for this source", backend.ID)
		case contextClasses[c.Class] && b.Record.Context != digest:
			c.Status = diag.StatusError
			c.Reason = fmt.Sprintf("backend %s judged this source under package context %s, not the reported %s", backend.ID, b.Record.Context, digest)
		default:
			mergeClass(f, c, digest, b.Predictions, b.Record.Withheld[c.Class], backend)
		}
	}
	sort.SliceStable(f.Diagnostics, func(i, j int) bool { return f.Diagnostics[i].Span.Start < f.Diagnostics[j].Span.Start })
}

// Unjudged candidates lower coverage without discarding supported findings.
// Count withheld candidates too, even when the backend could not give them a span.
func mergeClass(f *diag.FileReport, c *diag.Coverage, digest string, predictions []advisory.Prediction, withheld map[string]int, backend *diag.Backend) {
	count := 0
	for _, reason := range slices.Sorted(maps.Keys(withheld)) {
		for range withheld[reason] {
			count++
			c.Items = append(c.Items, diag.CoverageItem{Status: diag.StatusUnsupported, Reason: reason})
			c.Status = diag.StatusUnsupported
		}
	}
	for _, p := range predictions {
		if p.Class != c.Class {
			continue
		}
		count++
		if p.Coverage != advisory.StatusChecked {
			span := p.Span
			c.Items = append(c.Items, diag.CoverageItem{Span: &span, Status: p.Coverage, Reason: *p.Reason})
			if c.Status != diag.StatusError {
				c.Status = p.Coverage
			}
			continue
		}
		if *p.Advice {
			f.Diagnostics = append(f.Diagnostics, diag.Diagnostic{
				Class:         c.Class,
				Origin:        diag.OriginAdvisory,
				Path:          f.Path,
				SourceSHA256:  f.SHA256,
				ContextDigest: digest,
				Span:          p.Span,
				Message:       classNames[c.Class],
				Backend:       backend,
				Score:         p.Score,
			})
		}
	}
	c.Candidates = &count
	if len(c.Items) > 0 {
		c.Reason = fmt.Sprintf("%d candidate(s) could not be judged", len(c.Items))
	}
}
