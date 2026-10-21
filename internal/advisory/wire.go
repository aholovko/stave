// Package advisory defines the prediction protocol and binds responses to requested source revisions.
// Failed or missing responses remain distinct from "checked, no advice".
package advisory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"

	"github.com/aholovko/stave/internal/diag"
)

// Schema names the advisory wire format.
const Schema = "stave-advisory-v1"

// The statuses of a file record and of the coverage of a prediction.
const (
	StatusChecked     = "checked"
	StatusUnsupported = "unsupported"
	StatusError       = "error"
)

// Provider is one advisory backend.
type Provider interface {
	Predict(context.Context, Request) (Response, error)
}

// FileRef identifies one requested source revision.
type FileRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Request asks a backend about saved source revisions.
type Request struct {
	Schema string    `json:"schema"`
	Files  []FileRef `json:"files"`
}

// Backend identifies what produced a response.
type Backend struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
	Detail string `json:"detail"`
}

// FileRecord describes one source revision. Optional Path keeps recordings portable across checkouts.
// Nil Classes means every declared class; an explicit list limits coverage to those classes.
// Context is the backend's certified package digest, or empty when absent.
// Withheld counts candidates excluded before scoring, by class and reason, without spans.
type FileRecord struct {
	Path     string                    `json:"path,omitempty"`
	SHA256   string                    `json:"sha256"`
	Status   string                    `json:"status"`
	Reason   *string                   `json:"reason"`
	Classes  []string                  `json:"classes,omitzero"` // omits only nil; [] stays "none"
	Context  string                    `json:"context,omitempty"`
	Withheld map[string]map[string]int `json:"withheld,omitempty"`
}

// Checks reports whether a record judges class, which is a class that the backend declares.
func (r FileRecord) Checks(class string) bool {
	return r.Status == StatusChecked && (r.Classes == nil || slices.Contains(r.Classes, class))
}

// Prediction is the statement of the backend about one candidate span. Advice and Score are pointers because the
// contract tells null apart from a value.
type Prediction struct {
	Path     string    `json:"path,omitempty"`
	Class    string    `json:"class"`
	SHA256   string    `json:"sha256"`
	Span     diag.Span `json:"span"`
	Coverage string    `json:"coverage"`
	Advice   *bool     `json:"advice"`
	Score    *float64  `json:"score"`
	Reason   *string   `json:"reason"`
}

// Response is everything a backend says about one request.
type Response struct {
	Schema      string       `json:"schema"`
	Backend     Backend      `json:"backend"`
	Classes     []string     `json:"classes"`
	Files       []FileRecord `json:"files"`
	Predictions []Prediction `json:"predictions"`
}

// Decode accepts unknown fields for protocol extensions, but rejects trailing data: an exchange contains one object.
func Decode(r io.Reader) (Response, error) {
	dec := json.NewDecoder(r)
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("advisory response is not one JSON object: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Response{}, fmt.Errorf("advisory response has data after its JSON object")
	}
	return resp, nil
}

// Bound is what a response says about one requested file.
type Bound struct {
	// A nil Record means missing coverage, not a negative judgement.
	Record      *FileRecord
	Predictions []Prediction
}

// Resolve binds records by requested path. Any contract violation rejects the whole response rather than presenting
// a partially trusted answer as complete coverage.
func Resolve(req Request, resp Response) (map[string]Bound, error) {
	if resp.Schema != Schema {
		return nil, fmt.Errorf("advisory response schema is %q, want %q", resp.Schema, Schema)
	}
	if resp.Backend.ID == "" {
		return nil, fmt.Errorf("advisory response names no backend id")
	}
	served := map[string]bool{}
	for _, class := range resp.Classes {
		if !slices.Contains(diag.AdvisoryClasses, class) {
			return nil, fmt.Errorf("advisory backend declares class %q, which is not an advisory class", class)
		}
		served[class] = true
	}

	bound := map[string]Bound{}
	for _, f := range req.Files {
		bound[f.Path] = Bound{}
	}

	for _, rec := range resp.Files {
		switch rec.Status {
		case StatusChecked:
		case StatusUnsupported, StatusError:
			if rec.Reason == nil || *rec.Reason == "" {
				return nil, fmt.Errorf("file record %s is %s without a reason", identity(rec.Path, rec.SHA256), rec.Status)
			}
		default:
			return nil, fmt.Errorf("file record %s has status %q", identity(rec.Path, rec.SHA256), rec.Status)
		}
		if err := checkClasses(rec, served); err != nil {
			return nil, fmt.Errorf("file record %s: %w", identity(rec.Path, rec.SHA256), err)
		}
		targets, err := applicable(req, rec.Path, rec.SHA256)
		if err != nil {
			return nil, fmt.Errorf("file record: %w", err)
		}
		for _, path := range targets {
			b := bound[path]
			if b.Record != nil {
				if !sameRecord(*b.Record, rec) {
					return nil, fmt.Errorf("file records for %s contradict each other", path)
				}
				continue
			}
			rec := rec
			b.Record = &rec
			bound[path] = b
		}
	}

	for _, p := range resp.Predictions {
		where := fmt.Sprintf("%s prediction [%d,%d) for %s", p.Class, p.Span.Start, p.Span.End, identity(p.Path, p.SHA256))
		if !served[p.Class] {
			return nil, fmt.Errorf("%s: the backend does not declare that class", where)
		}
		if p.Span.Start < 0 || p.Span.Start >= p.Span.End {
			return nil, fmt.Errorf("%s: a span is nonempty and half-open", where)
		}
		switch p.Coverage {
		case StatusChecked:
			if p.Advice == nil {
				return nil, fmt.Errorf("%s: checked requires boolean advice", where)
			}
			if p.Score != nil && (math.IsNaN(*p.Score) || math.IsInf(*p.Score, 0)) {
				return nil, fmt.Errorf("%s: score is not finite", where)
			}
		case StatusUnsupported, StatusError:
			if p.Advice != nil || p.Score != nil {
				return nil, fmt.Errorf("%s: %s carries null advice and a null score", where, p.Coverage)
			}
			if p.Reason == nil || *p.Reason == "" {
				return nil, fmt.Errorf("%s: %s without a reason", where, p.Coverage)
			}
		default:
			return nil, fmt.Errorf("%s: coverage %q", where, p.Coverage)
		}
		targets, err := applicable(req, p.Path, p.SHA256)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		for _, path := range targets {
			b := bound[path]
			if b.Record == nil || b.Record.Status != StatusChecked {
				return nil, fmt.Errorf("%s: its file has no checked record", where)
			}
			if !b.Record.Checks(p.Class) {
				return nil, fmt.Errorf("%s: its file record does not check that class", where)
			}
			duplicate := false
			for _, prior := range b.Predictions {
				if prior.Class != p.Class || prior.Span != p.Span {
					continue
				}
				if !samePrediction(prior, p) {
					return nil, fmt.Errorf("%s: contradicts another prediction for the same candidate", where)
				}
				duplicate = true
			}
			if !duplicate {
				b.Predictions = append(b.Predictions, p)
				bound[path] = b
			}
		}
	}
	return bound, nil
}

// A pathless record applies to every requested file with its hash. A path-qualified record must match both fields.
func applicable(req Request, path, sha string) ([]string, error) {
	var out []string
	for _, f := range req.Files {
		if f.SHA256 == sha && (path == "" || f.Path == path) {
			out = append(out, f.Path)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s was not requested", identity(path, sha))
	}
	return out, nil
}

func identity(path, sha string) string {
	if len(sha) > 12 {
		sha = sha[:12]
	}
	if path == "" {
		return "sha256 " + sha
	}
	return path + " at sha256 " + sha
}

// checkClasses checks that the classes and withheld counts of a record stay within the classes that the backend
// declares. Only a checked record names classes or counts candidates.
func checkClasses(rec FileRecord, served map[string]bool) error {
	if rec.Status != StatusChecked && (rec.Classes != nil || rec.Context != "" || rec.Withheld != nil) {
		return fmt.Errorf("a %s record names no classes, context or withheld counts", rec.Status)
	}
	for _, class := range rec.Classes {
		if !served[class] {
			return fmt.Errorf("checks class %q, which the backend does not declare", class)
		}
	}
	for class, counts := range rec.Withheld {
		if !served[class] || !rec.Checks(class) {
			return fmt.Errorf("withholds %s candidates, a class it does not check", class)
		}
		for reason, n := range counts {
			if reason == "" || n < 1 {
				return fmt.Errorf("withholds %d %s candidates for reason %q; a count is positive and has a reason", n, class, reason)
			}
		}
	}
	return nil
}

func sameRecord(a, b FileRecord) bool {
	return a.Status == b.Status && equal(a.Reason, b.Reason) && slices.Equal(a.Classes, b.Classes) &&
		(a.Classes == nil) == (b.Classes == nil) && a.Context == b.Context &&
		maps.EqualFunc(a.Withheld, b.Withheld, maps.Equal)
}

func samePrediction(a, b Prediction) bool {
	return a.Coverage == b.Coverage && equal(a.Advice, b.Advice) && equal(a.Score, b.Score) && equal(a.Reason, b.Reason)
}

func equal[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
