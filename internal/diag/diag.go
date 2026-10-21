// Package diag defines reports shared by the CLI and language server without depending on analyzers.
// Findings retain original source byte spans so each presenter can derive its own display positions.
package diag

// Schema names the package report format.
const Schema = "stave-report-v1"

// Origins of a finding or a coverage statement.
const (
	OriginExact    = "exact"
	OriginAdvisory = "advisory"
)

// File roles, mirrored from the snapshot. A test target is a _test.go file analysed only on request. It is never part
// of the package context.
const (
	RoleTarget      = "target"
	RoleTestTarget  = "test-target"
	RoleContextOnly = "context-only"
	RoleIgnored     = "ignored"
)

// Coverage statuses. Only StatusChecked lets an absent finding be read as a negative. Unsupported, error and disabled
// each say that the class was not judged.
const (
	StatusChecked     = "checked"
	StatusUnsupported = "unsupported"
	StatusError       = "error"
	StatusDisabled    = "disabled"
)

// ExactClasses and AdvisoryClasses define report order. Exact and advisory classes
// can share a stable ID, so origin is also needed to distinguish them.
var (
	ExactClasses    = []string{"P1", "P2", "P3"}
	AdvisoryClasses = []string{"P1", "P2", "P3", "TF", "DC", "RP"}
)

// Span is a half-open byte range [Start, End) into the original source bytes.
type Span struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Backend identifies what produced a finding: the exact analyzer, or the advisory backend that a provider reached.
type Backend struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
	Detail string `json:"detail"`
}

// Diagnostic identifies a finding by class, origin, path, source hash, context digest and original byte span.
// Score is optional, backend-specific and not a calibrated probability.
type Diagnostic struct {
	Class         string   `json:"class"`
	Origin        string   `json:"origin"`
	Path          string   `json:"path"`
	SourceSHA256  string   `json:"source_sha256"`
	ContextDigest string   `json:"context_digest"`
	Span          Span     `json:"span"`
	Message       string   `json:"message"`
	Backend       *Backend `json:"backend"`
	Score         *float64 `json:"score"`
}

// CoverageItem is one detail behind a coverage status: a sibling that made the context incomplete, or a candidate that
// a backend could not judge.
type CoverageItem struct {
	Span   *Span  `json:"span,omitempty"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// Coverage says whether one class was judged for one file. It is separate from the diagnostics on purpose: a class may
// be unsupported while the findings it could support are still reported.
type Coverage struct {
	Class  string         `json:"class"`
	Origin string         `json:"origin"`
	Status string         `json:"status"`
	Reason string         `json:"reason"`
	Items  []CoverageItem `json:"items"`
	// Candidates counts reported candidates, including negatives and unjudged ones.
	// Duplicates count once. Nil means no count applies; zero means the backend
	// reported no candidates. This distinction prevents missing coverage from
	// looking like a successful check.
	Candidates *int `json:"candidates,omitempty"`
}

// FileReport is everything the system says about one file of the package. Only a target carries coverage. Any other
// role makes no claim about any class.
type FileReport struct {
	Name        string       `json:"name"`
	Path        string       `json:"path"`
	SHA256      string       `json:"sha256"`
	Package     string       `json:"package"`
	Role        string       `json:"role"`
	Reason      string       `json:"reason"`
	Coverage    []Coverage   `json:"coverage"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// Context identifies the sibling evidence the package was analysed under.
type Context struct {
	Digest     string   `json:"digest"`
	Complete   bool     `json:"complete"`
	Incomplete []string `json:"incomplete"`
}

// Build is the build selection the targets were chosen under, and the toolchain whose parser and build-constraint rules
// were applied.
type Build struct {
	GOOS   string   `json:"goos"`
	GOARCH string   `json:"goarch"`
	Tags   []string `json:"tags"`
	Cgo    bool     `json:"cgo"`
	Go     string   `json:"go"`
}

// AdvisoryOK is the advisory status of a completed exchange. Whether any class was judged for any file is still a
// matter for the coverage of each file.
const AdvisoryOK = "ok"

// Advisory says how the advisory exchange for the package went: AdvisoryOK, StatusDisabled or StatusError.
type Advisory struct {
	Provider string   `json:"provider"`
	Status   string   `json:"status"`
	Reason   string   `json:"reason"`
	Backend  *Backend `json:"backend"`
	// Classes are the advisory classes that the backend declared it serves. A class outside this list is unsupported
	// everywhere. That is a fact about the backend, not about any one file.
	Classes []string `json:"classes"`
}

// PackageReport is the stave-report-v1 object: one per package directory.
type PackageReport struct {
	Schema   string       `json:"schema"`
	Dir      string       `json:"dir"`
	Build    Build        `json:"build"`
	Context  Context      `json:"context"`
	Advisory Advisory     `json:"advisory"`
	Files    []FileReport `json:"files"`
}
