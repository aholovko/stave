// Package analyzer reports P1, P2 and P3 using exact checks.
// Conservative checks avoid reporting ambiguous cases as violations.
package analyzer

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"

	"github.com/aholovko/stave/internal/instance"
	"github.com/aholovko/stave/internal/span"
)

// rules contains the three supported exact checks in a fixed order so errors are reproducible.
var rules = []struct {
	class string
	run   func(*scan)
}{
	{"P1", (*scan).checkElseAfterReturn},
	{"P2", (*scan).checkErrorStrings},
	{"P3", (*scan).checkDeclaredNames},
}

type scan struct {
	inst *instance.Instance
	src  []byte
	file *ast.File
	out  []span.Occurrence
	err  error
}

// emit validates a nonempty source span before recording a finding. A construction error aborts the analysis.
func (s *scan) emit(class string, start, end int) {
	if s.err != nil {
		return
	}
	if start < 0 || end < start || end > len(s.src) {
		s.err = fmt.Errorf("%s: malformed anchor [%d,%d) over %d bytes", class, start, end, len(s.src))
		return
	}
	o, err := span.NewOccurrence(class, span.Span{Start: start, End: end}, span.AffNone)
	if err != nil {
		s.err = err
		return
	}
	s.out = append(s.out, o)
}

func (s *scan) at(p token.Pos) int { return s.inst.Offset(p) }

func (s *scan) emitNode(class string, n ast.Node) {
	s.emit(class, s.at(n.Pos()), s.at(n.End()))
}

// Analyze returns P1, P2 and P3 occurrences in ascending anchor order. Contract violations return an error
// and no partial result.
func Analyze(inst *instance.Instance) ([]span.Occurrence, error) {
	if inst == nil {
		return nil, fmt.Errorf("analyze: nil instance")
	}
	s := &scan{inst: inst, src: inst.Source(), file: inst.File()}

	for _, r := range rules {
		r.run(s)
		if s.err != nil {
			return nil, fmt.Errorf("analyze %s: %w", r.class, s.err)
		}
	}

	sort.Slice(s.out, func(i, j int) bool {
		a, b := s.out[i], s.out[j]
		if a.Span.Start != b.Span.Start {
			return a.Span.Start < b.Span.Start
		}
		if a.Span.End != b.Span.End {
			return a.Span.End < b.Span.End
		}
		return a.Class < b.Class
	})
	if err := checkInjective(s.out); err != nil {
		return nil, err
	}
	return s.out, nil
}

// checkInjective rejects duplicate identities before set comparisons can silently lose occurrences.
func checkInjective(out []span.Occurrence) error {
	seen := make(map[span.Occurrence]bool, len(out))
	for _, o := range out {
		if seen[o] {
			return fmt.Errorf("analyze: duplicate occurrence identity %s; the anchor convention is not injective", o)
		}
		seen[o] = true
	}
	return nil
}
