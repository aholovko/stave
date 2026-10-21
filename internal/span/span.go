// Package span represents byte ranges and finding locations.
package span

import "fmt"

// Affinity identifies the side of a zero-length anchor. Nonzero spans use AffNone.
type Affinity uint8

const (
	AffNone Affinity = iota
	AffLeft
	AffRight
)

func (a Affinity) String() string {
	switch a {
	case AffLeft:
		return "left"
	case AffRight:
		return "right"
	default:
		return "none"
	}
}

// Span is a half-open byte range with Start <= End. Zero-length ranges represent anchors.
type Span struct {
	Start, End int
}

func (s Span) Zero() bool { return s.Start == s.End }

func (s Span) Valid() bool { return s.Start >= 0 && s.Start <= s.End }

func (s Span) String() string { return fmt.Sprintf("[%d,%d)", s.Start, s.End) }

// Occurrence identifies a finding by class, span, and affinity.
type Occurrence struct {
	Class string
	Span  Span
	Aff   Affinity
}

// NewOccurrence requires an affinity for zero-length anchors and none for nonzero spans.
func NewOccurrence(class string, s Span, aff Affinity) (Occurrence, error) {
	if class == "" {
		return Occurrence{}, fmt.Errorf("occurrence: empty class")
	}
	if !s.Valid() {
		return Occurrence{}, fmt.Errorf("occurrence %s: malformed span %s", class, s)
	}
	if s.Zero() && aff == AffNone {
		return Occurrence{}, fmt.Errorf("occurrence %s: zero-length anchor at %d needs an affinity", class, s.Start)
	}
	if !s.Zero() && aff != AffNone {
		return Occurrence{}, fmt.Errorf("occurrence %s: nonzero span %s must not carry affinity %s", class, s, aff)
	}
	return Occurrence{Class: class, Span: s, Aff: aff}, nil
}

func (o Occurrence) String() string {
	return fmt.Sprintf("%s@%s/%s", o.Class, o.Span, o.Aff)
}
