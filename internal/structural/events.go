// Package structural builds and validates event streams that describe candidate
// syntax. The selector derives roles and byte ownership from events alone, keeping
// structural features independent of verdicts, labels and assessor text. All spans
// use half-open offsets into the original source bytes.
package structural

import "encoding/json"

// Header opens every event stream. Len and SHA256 identify the source bytes.
type Header struct {
	Task   string `json:"task"`
	Cand   string `json:"cand"`
	Len    int    `json:"len"`
	SHA256 string `json:"sha256"`
}

// Event is one of OPEN, CLOSE, SLOT, LEAF, CAND, MARK (TF only) or BAD. K names the frame, leaf or marker kind. N is
// the slot name. S and T are the span.
type Event struct {
	E      string `json:"e"`
	K      string `json:"k,omitempty"`
	N      string `json:"n,omitempty"`
	S      int    `json:"s"`
	T      int    `json:"t"`
	Single bool   `json:"single,omitempty"`
	ID     string `json:"id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Stream is one candidate's header and events.
type Stream struct {
	Header Header
	Events []Event
}

// Source is the recovery identity of the selected source bytes.
type Source struct {
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Role is a sorted disjoint interval union with its status: present or structural_empty.
type Role struct {
	IV  [][2]int `json:"iv"`
	Why string   `json:"why"`
}

// Fact describes one owned interval. Empty Slot/Last and negative Ordinal/Depth serialize as null.
type Fact struct {
	S, T         int
	Kind, Parent string
	Slot         string
	Ordinal      int
	Last         string
	Depth        int
	Lit          int
}

// MarshalJSON writes [s,t,kind,parent,slot,ordinal,last,depth,litdepth].
func (f Fact) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{f.S, f.T, f.Kind, f.Parent, nullString(f.Slot), nullInt(f.Ordinal),
		nullString(f.Last), nullInt(f.Depth), f.Lit})
}

// Result is the selector outcome for one stream. A failed result carries no runs, roles or facts.
type Result struct {
	OK         bool            `json:"ok"`
	Task       string          `json:"task"`
	Cand       string          `json:"cand"`
	Source     Source          `json:"source"`
	Runs       [][2]int        `json:"runs,omitempty"`
	Roles      map[string]Role `json:"roles,omitempty"`
	Facts      []Fact          `json:"facts,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	EventIndex *int            `json:"event_index,omitempty"`
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int) any {
	if n < 0 {
		return nil
	}
	return n
}
