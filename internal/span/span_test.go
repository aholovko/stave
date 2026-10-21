package span

import "testing"

func TestNewOccurrenceEnforcesTheAffinityPairing(t *testing.T) {
	if _, err := NewOccurrence("test", Span{5, 5}, AffNone); err == nil {
		t.Fatal("a zero-length anchor without an affinity must be rejected")
	}
	if _, err := NewOccurrence("test", Span{4, 9}, AffLeft); err == nil {
		t.Fatal("a nonzero span carrying an affinity must be rejected")
	}
	if _, err := NewOccurrence("test", Span{5, 5}, AffRight); err != nil {
		t.Fatalf("a well-formed anchor must be accepted: %v", err)
	}
	if _, err := NewOccurrence("test", Span{4, 9}, AffNone); err != nil {
		t.Fatalf("a well-formed nonzero span must be accepted: %v", err)
	}
}
