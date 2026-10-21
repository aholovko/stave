// Package lsp serves saved-source analysis over stdio. Buffers must match disk because findings bind the saved source
// and its sibling context. Stdout is reserved for protocol frames.
package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/aholovko/stave/internal/app"
	"github.com/aholovko/stave/internal/diag"
	"github.com/aholovko/stave/internal/harness"
	"github.com/aholovko/stave/internal/pkgsnap"
)

// Serve returns 0 after an orderly shutdown, 1 otherwise. Operator messages go to logw.
// configure is called for each analysis so model changes take effect without a restart.
func Serve(configure func() harness.Config, in io.Reader, out io.Writer, logw io.Writer) int {
	ctx, cancel := context.WithCancel(context.Background())
	s := &server{
		configure: configure,
		out:       &frameWriter{w: out},
		logw:      logw,
		ctx:       ctx,
		cancel:    cancel,
		enc:       "utf-16",
		docs:      map[string]*document{},
		stated:    map[string]bool{},
	}
	code := s.loop(bufio.NewReader(in))
	// Reap every backend before exit. Workers need the document mutex to finish, so join without holding it.
	s.cancel()
	s.workers.Wait()
	return code
}

func (s *server) loop(r *bufio.Reader) int {
	for {
		body, err := readFrame(r)
		if err != nil {
			if s.shutdown {
				return 0
			}
			fmt.Fprintf(s.logw, "stave lsp: input ended before exit: %v\n", err)
			return 1
		}
		if code, done := s.handle(body); done {
			return code
		}
	}
}

// identity binds an asynchronous result to its source, sibling context and configuration.
type identity struct {
	sha, digest, key string
}

// document tracks one open buffer. Versions distinguish identical text after an undo, while pointer identity
// distinguishes a closed and reopened document whose version may repeat.
type document struct {
	uri     string
	path    string // empty for a document that is not a file
	version int
	text    []byte

	published        identity // zero after a clearing publication
	publishedVersion int
	cleared          bool // the last publication was empty
}

type server struct {
	configure func() harness.Config
	out       *frameWriter
	logw      io.Writer
	ctx       context.Context
	cancel    context.CancelFunc
	workers   sync.WaitGroup // advisory exchanges, running or queued

	mu          sync.Mutex // guards everything below
	initialized bool
	shutdown    bool
	enc         string
	docs        map[string]*document
	stated      map[string]bool // one-time notes already sent

	// Serialize model loads to limit peak memory.
	adviseMu sync.Mutex
}

// status distinguishes a finished analysis from an empty diagnostic set. Clients must show pending for a different
// document version. Source and context hashes are absent when nothing was analysed.
type status struct {
	URI     string   `json:"uri"`
	Version int      `json:"version"`
	State   string   `json:"state"`
	Summary string   `json:"summary"`
	Detail  []string `json:"detail"`

	SourceSHA256  string `json:"source_sha256,omitempty"`
	ContextDigest string `json:"context_digest,omitempty"`
}

const (
	stateRunning     = "running"      // exact findings published, advisory exchange pending
	stateComplete    = "complete"     // everything for this version is published
	stateUnsaved     = "unsaved"      // the buffer is not the saved file, so nothing is analysed
	stateNotAnalysed = "not-analysed" // the document is not a target
	stateFailed      = "failed"       // the analysis, the advisory exchange or a class of this file failed
)

// The caller holds s.mu.
func (s *server) status(d *document, state, summary string, detail []string) {
	if detail == nil {
		detail = []string{}
	}
	s.notify("stave/status", status{URI: d.uri, Version: d.version, State: state, Summary: summary, Detail: detail,
		SourceSHA256: d.published.sha, ContextDigest: d.published.digest})
}

type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	codeMethodNotFound   = -32601
	codeInvalidParams    = -32602
	codeNotInitialized   = -32002
	logError, logWarning = 1, 2
	logInfo              = 3
)

type lspDiagnostic struct {
	Range    lspRange         `json:"range"`
	Severity int              `json:"severity"`
	Code     string           `json:"code"`
	Source   string           `json:"source"`
	Message  string           `json:"message"`
	Data     *diag.Diagnostic `json:"data,omitempty"`
}

// handle returns done after exit, with the process exit code.
func (s *server) handle(body []byte) (code int, done bool) {
	var msg message
	if err := json.Unmarshal(body, &msg); err != nil {
		fmt.Fprintf(s.logw, "stave lsp: dropping a message that is not JSON-RPC: %v\n", err)
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case msg.Method == "":
		// A response to a server request. This server sends no requests.
	case len(msg.ID) > 0 && string(msg.ID) != "null":
		result, err := s.request(msg.Method, msg.Params)
		s.respond(msg.ID, result, err)
	case msg.Method == "exit":
		if s.shutdown {
			return 0, true
		}
		return 1, true
	default:
		s.notification(msg.Method, msg.Params)
	}
	return 0, false
}

func (s *server) request(method string, params json.RawMessage) (any, *rpcError) {
	if !s.initialized && method != "initialize" {
		return nil, &rpcError{codeNotInitialized, "the server is not initialized"}
	}
	switch method {
	case "initialize":
		var p struct {
			Capabilities struct {
				General struct {
					PositionEncodings []string `json:"positionEncodings"`
				} `json:"general"`
			} `json:"capabilities"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{codeInvalidParams, err.Error()}
		}
		s.initialized = true
		s.enc = chooseEncoding(p.Capabilities.General.PositionEncodings)
		return map[string]any{
			"capabilities": map[string]any{
				"positionEncoding": s.enc,
				"textDocumentSync": map[string]any{
					"openClose": true,
					"change":    1, // full content on every change
					"save":      map[string]any{"includeText": false},
				},
			},
			"serverInfo": map[string]any{"name": "stave", "version": app.Version},
		}, nil
	case "shutdown":
		s.shutdown = true
		return nil, nil
	}
	return nil, &rpcError{codeMethodNotFound, "method not found: " + method}
}

func (s *server) notification(method string, params json.RawMessage) {
	var p struct {
		TextDocument struct {
			URI     string `json:"uri"`
			Version int    `json:"version"`
			Text    string `json:"text"`
		} `json:"textDocument"`
		ContentChanges []struct {
			Range *lspRange `json:"range"`
			Text  string    `json:"text"`
		} `json:"contentChanges"`
		Changes []struct {
			URI string `json:"uri"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
		fmt.Fprintf(s.logw, "stave lsp: %s: %v\n", method, err)
		return
	}
	switch method {
	case "initialized":
		s.state()
	case "textDocument/didOpen":
		d := &document{uri: p.TextDocument.URI, version: p.TextDocument.Version, text: []byte(p.TextDocument.Text)}
		d.path, _ = pathOf(d.uri)
		s.docs[d.uri] = d
		s.refresh(d)
	case "textDocument/didChange":
		d := s.docs[p.TextDocument.URI]
		if d == nil {
			return
		}
		d.version = p.TextDocument.Version
		for _, change := range p.ContentChanges {
			if change.Range != nil {
				// We cannot reconstruct a partial change after negotiating full synchronization.
				s.log(logWarning, "%s: an incremental change was received; the buffer is treated as unsaved until it is opened again", filepath.Base(d.path))
				d.text = nil
				s.clear(d, stateUnsaved, "unsaved: the buffer is unknown until the file is reopened", nil)
				return
			}
			d.text = []byte(change.Text)
		}
		s.refresh(d)
	case "textDocument/didSave":
		if d := s.docs[p.TextDocument.URI]; d != nil {
			s.refresh(d)
		}
	case "textDocument/didClose":
		if d := s.docs[p.TextDocument.URI]; d != nil {
			delete(s.docs, d.uri)
			s.publish(d.uri, nil, []lspDiagnostic{})
		}
	case "workspace/didChangeWatchedFiles":
		var dirs []string
		for _, change := range p.Changes {
			if path, ok := pathOf(change.URI); ok {
				dirs = append(dirs, filepath.Dir(path))
			}
		}
		for _, d := range s.docs {
			if d.path != "" && slices.ContainsFunc(dirs, func(dir string) bool {
				return sameDir(dir, filepath.Dir(d.path), runtime.GOOS)
			}) {
				s.refresh(d)
			}
		}
	}
}

func (s *server) state() {
	if cfg := s.configure(); cfg.Provider == nil {
		s.log(logInfo, "advisory analysis is disabled; exact classes P1-P3 only")
	} else {
		s.log(logInfo, "advisory provider: %s", providerName(cfg.Provider))
	}
}

func providerName(p any) string {
	if named, ok := p.(interface{ Name() string }); ok {
		return named.Name()
	}
	return "custom"
}

// refresh shares the saved-source checks across document and watcher events. The caller holds s.mu.
func (s *server) refresh(d *document) {
	if d.path == "" {
		s.clear(d, stateNotAnalysed, "not analysed: not a file on disk", nil)
		return
	}
	if d.text == nil {
		s.clear(d, stateUnsaved, "unsaved: the buffer is unknown until the file is reopened", nil)
		return
	}
	// Check the buffer before the package so typing costs only one file read. A mismatch can also come from editor
	// decoding, such as stripping a byte order mark, so the status must not assume an unsaved edit.
	disk, err := os.ReadFile(d.path)
	if err != nil {
		s.clear(d, stateFailed, "the saved file cannot be read", []string{err.Error()})
		return
	}
	if !bytes.Equal(disk, d.text) {
		s.clear(d, stateUnsaved, "buffer differs from the saved file (only the saved file is analysed)", nil)
		return
	}
	cfg := s.configure()
	dir, name := filepath.Split(d.path)
	snap, err := pkgsnap.Take(dir, cfg.Build, []string{name})
	if err != nil {
		s.log(logError, "%s: %v", name, err)
		s.clear(d, stateFailed, "analysis failed", []string{err.Error()})
		return
	}
	f, _ := snap.File(name)
	if f.Role != pkgsnap.RoleTarget && f.Role != pkgsnap.RoleTestTarget {
		s.clear(d, stateNotAnalysed, fmt.Sprintf("not analysed (%s): %s", f.Role, f.Reason), nil)
		s.once(d.uri+"\x00"+f.Reason, logInfo, "%s is not analysed (%s): %s", name, f.Role, f.Reason)
		return
	}
	if !bytes.Equal(f.Src, d.text) {
		// The file changed between the two reads.
		s.clear(d, stateUnsaved, "buffer differs from the saved file (only the saved file is analysed)", []string{"the file changed while its package was read"})
		return
	}
	id := identity{sha: f.SHA256, digest: snap.Context.Digest(), key: cfg.Key}
	if d.published == id && d.publishedVersion == d.version {
		return // the client already holds the result of this version, source, context and configuration
	}
	rep := harness.Report(snap)
	d.published, d.publishedVersion, d.cleared = id, d.version, false
	running := ""
	if cfg.Provider != nil {
		running = providerName(cfg.Provider)
	} else {
		// Fill in disabled coverage without starting a worker.
		rep = cfg.Advise(s.ctx, []*pkgsnap.Snapshot{snap}, []diag.PackageReport{rep})[0]
	}
	s.publish(d.uri, &d.version, s.render(d.text, f.PackageSpan, rep, name))
	state, summary, detail := s.summarize(rep, name, running)
	s.status(d, state, summary, detail)
	if cfg.Provider != nil {
		s.workers.Add(1)
		go s.advise(cfg, d, d.version, id, snap, rep)
	}
}

// Repeat the status even when diagnostics are already empty: each edit has a new version.
func (s *server) clear(d *document, state, summary string, detail []string) {
	d.published = identity{}
	if !d.cleared {
		d.cleared = true
		s.publish(d.uri, &d.version, []lspDiagnostic{})
	}
	s.status(d, state, summary, detail)
}

// advise checks freshness before and after the exchange. Queued work may become stale while another backend runs.
func (s *server) advise(cfg harness.Config, d *document, version int, id identity, snap *pkgsnap.Snapshot, rep diag.PackageReport) {
	defer s.workers.Done()
	s.adviseMu.Lock()
	defer s.adviseMu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	if !s.current(d, version, id) {
		s.log(logInfo, "advisory exchange for %s version %d skipped: the document moved on before it started", filepath.Base(d.uri), version)
		return
	}
	merged := cfg.Advise(s.ctx, []*pkgsnap.Snapshot{snap}, []diag.PackageReport{rep})[0]
	if s.ctx.Err() != nil {
		return // the shutdown cut the exchange short, and nobody is listening
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.docs[d.uri] != d || d.version != version || d.published != id {
		s.log(logInfo, "advisory result for %s version %d dropped: the document moved on", filepath.Base(d.uri), version)
		return
	}
	if merged.Advisory.Status == diag.StatusError {
		s.log(logError, "advisory provider %s failed: %s", merged.Advisory.Provider, merged.Advisory.Reason)
	}
	name := filepath.Base(d.path)
	f, _ := snap.File(name)
	s.publish(d.uri, &d.version, s.render(d.text, f.PackageSpan, merged, name))
	state, summary, detail := s.summarize(merged, name, "")
	s.status(d, state, summary, detail)
	// Allow an unchanged save to retry a failure while keeping the diagnostics visible.
	if merged.Advisory.Status == diag.StatusError || advisoryError(merged, name) {
		d.published = identity{}
	}
}

func advisoryError(rep diag.PackageReport, name string) bool {
	for _, f := range rep.Files {
		if f.Name != name {
			continue
		}
		for _, c := range f.Coverage {
			if c.Origin == diag.OriginAdvisory && c.Status == diag.StatusError {
				return true
			}
		}
	}
	return false
}

func (s *server) current(d *document, version int, id identity) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.docs[d.uri] == d && d.version == version && d.published == id
}

// summarize keeps completion separate from coverage: an unjudged class must not look clean.
// running names the pending provider because the exact report still contains placeholder advisory coverage.
func (s *server) summarize(rep diag.PackageReport, name string, running string) (state, summary string, detail []string) {
	var fr diag.FileReport
	for _, f := range rep.Files {
		if f.Name == name {
			fr = f
		}
	}
	exact, advisedBy := 0, map[string]int{}
	for _, d := range fr.Diagnostics {
		if d.Origin == diag.OriginExact {
			exact++
		} else {
			advisedBy[d.Class]++
		}
	}
	var exactCov, advisoryCov []diag.Coverage
	for _, c := range fr.Coverage {
		switch {
		case c.Origin == diag.OriginExact:
			exactCov = append(exactCov, c)
		case rep.Advisory.Status == diag.AdvisoryOK && !slices.Contains(rep.Advisory.Classes, c.Class):
			// An unserved class says nothing about this file.
		default:
			advisoryCov = append(advisoryCov, c)
		}
	}

	state = stateComplete
	summary = plural(len(fr.Diagnostics), "finding")
	detail = []string{"P1–P3 exact: " + plural(exact, "finding")}
	detail = append(detail, diag.NotJudged(exactCov)...)
	if failed := inError(exactCov); len(failed) > 0 {
		state = stateFailed
		summary += "; " + strings.Join(failed, " ") + " exact error"
	}
	if unjudged := notJudged(exactCov); len(unjudged) > 0 {
		summary += "; " + strings.Join(unjudged, " ") + " exact not judged"
	}

	switch {
	case running != "":
		state = stateRunning
		summary += "; advisory running"
		detail = append(detail, fmt.Sprintf("advisory analysis: running (%s provider)", running))
	case rep.Advisory.Status == diag.StatusDisabled:
		summary += "; advisory disabled"
		detail = append(detail, "advisory classes disabled: "+rep.Advisory.Reason)
	case rep.Advisory.Status == diag.StatusError:
		state = stateFailed
		summary += "; advisory failed"
		detail = append(detail, "advisory failed: "+rep.Advisory.Reason)
	default:
		var parts []string
		for _, c := range advisoryCov {
			if line := diag.Candidates(c, advisedBy[c.Class]); line != "" {
				detail = append(detail, line)
				parts = append(parts, line)
			} else if c.Status == diag.StatusError {
				parts = append(parts, c.Class+" error")
			} else {
				parts = append(parts, c.Class+" not judged")
			}
		}
		if len(inError(advisoryCov)) > 0 {
			state = stateFailed
		}
		if len(parts) == 0 {
			parts = []string{"advisory not judged"}
		}
		summary += "; " + strings.Join(parts, "; ")
		if b := rep.Advisory.Backend; b != nil {
			detail = append(detail, fmt.Sprintf("backend %s (%s), digest %s", b.ID, b.Detail, abbrev(b.Digest)))
		}
		detail = append(detail, diag.NotJudged(advisoryCov)...)
	}
	detail = append(detail, fmt.Sprintf("saved source %s, context %s", abbrev(fr.SHA256), abbrev(rep.Context.Digest)))
	return state, summary, detail
}

func inError(coverage []diag.Coverage) []string {
	var out []string
	for _, c := range coverage {
		if c.Status == diag.StatusError {
			out = append(out, c.Class)
		}
	}
	return out
}

func notJudged(coverage []diag.Coverage) []string {
	var out []string
	for _, c := range coverage {
		if c.Status != diag.StatusChecked && c.Status != diag.StatusError {
			out = append(out, c.Class)
		}
	}
	return out
}

func plural(n int, noun string) string {
	switch n {
	case 0:
		return "no " + noun + "s"
	case 1:
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func abbrev(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// File-specific coverage gaps belong in diagnostics. Disabled or unserved classes belong in logs because they say
// nothing about this particular file.
func (s *server) render(text []byte, pkgSpan [2]int, rep diag.PackageReport, name string) []lspDiagnostic {
	ix := newLineIndex(text, s.enc)
	out := []lspDiagnostic{}
	var fr diag.FileReport
	for _, f := range rep.Files {
		if f.Name == name {
			fr = f
		}
	}
	for i := range fr.Diagnostics {
		d := &fr.Diagnostics[i]
		// Exact and learned P1-P3 share labels, so the message also names the origin.
		item := lspDiagnostic{
			Range: ix.lspRange(d.Span.Start, d.Span.End), Severity: 2,
			Code: d.Class, Source: "stave", Message: d.Message, Data: d,
		}
		if d.Origin == diag.OriginAdvisory {
			item.Severity = 3
			item.Message += " (advisory"
			if d.Backend != nil {
				item.Message += ": " + d.Backend.ID
			}
			if d.Score != nil {
				item.Message += fmt.Sprintf(", score %.2f", *d.Score)
			}
			item.Message += ")"
		} else {
			item.Message += " (exact)"
		}
		out = append(out, item)
	}

	var gaps []diag.Coverage
	for _, c := range fr.Coverage {
		switch {
		case c.Status == diag.StatusChecked || c.Status == diag.StatusDisabled:
		case c.Origin == diag.OriginAdvisory && rep.Advisory.Status == diag.AdvisoryOK && !slices.Contains(rep.Advisory.Classes, c.Class):
			s.once("unserved\x00"+rep.Advisory.Provider+"\x00"+c.Class, logInfo, "the %s advisory provider does not serve %s", rep.Advisory.Provider, c.Class)
		default:
			gaps = append(gaps, c)
		}
	}
	for _, line := range diag.NotJudged(gaps) {
		out = append(out, lspDiagnostic{
			Range: ix.lspRange(pkgSpan[0], pkgSpan[1]), Severity: 3,
			Code: "coverage", Source: "stave", Message: "not judged: " + line,
		})
	}
	return out
}

func pathOf(uri string) (string, bool) { return pathOfFor(uri, runtime.GOOS) }

// Windows file URIs need drive-letter and network-share handling before use as filesystem paths.
func pathOfFor(uri, goos string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || u.Path == "" {
		return "", false
	}
	if goos != "windows" {
		return u.Path, true
	}
	p := u.Path
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	p = strings.ReplaceAll(p, "/", `\`)
	if u.Host != "" {
		p = `\\` + u.Host + p
	}
	return p, true
}

// Watcher events and document URIs may differ in case on Windows.
func sameDir(a, b, goos string) bool {
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Callers hold s.mu or own the document exclusively; frameWriter serializes the protocol bytes.
func (s *server) publish(uri string, version *int, diagnostics []lspDiagnostic) {
	params := map[string]any{"uri": uri, "diagnostics": diagnostics}
	if version != nil {
		params["version"] = *version
	}
	s.notify("textDocument/publishDiagnostics", params)
}

func (s *server) log(kind int, format string, args ...any) {
	s.notify("window/logMessage", map[string]any{"type": kind, "message": "stave: " + fmt.Sprintf(format, args...)})
}

func (s *server) once(key string, kind int, format string, args ...any) {
	if s.stated[key] {
		return
	}
	s.stated[key] = true
	s.log(kind, format, args...)
}

func (s *server) notify(method string, params any) {
	s.out.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (s *server) respond(id json.RawMessage, result any, err *rpcError) {
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if err != nil {
		msg["error"] = err
	} else {
		msg["result"] = result
	}
	s.out.write(msg)
}

type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (fw *frameWriter) write(msg any) {
	body, err := json.Marshal(msg)
	if err != nil {
		return
	}
	fw.mu.Lock()
	defer fw.mu.Unlock()
	fmt.Fprintf(fw.w, "Content-Length: %d\r\n\r\n", len(body))
	fw.w.Write(body)
}

// readFrame reads one Content-Length framed message.
func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			if _, err := fmt.Sscanf(strings.TrimSpace(value), "%d", &length); err != nil {
				return nil, fmt.Errorf("bad Content-Length %q", value)
			}
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("a frame without Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}
