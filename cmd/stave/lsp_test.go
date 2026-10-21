package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/aholovko/stave/internal/advisory"
	"github.com/aholovko/stave/internal/app"
	"github.com/aholovko/stave/internal/diag"
	"github.com/aholovko/stave/internal/harness"
	"github.com/aholovko/stave/internal/lsp"
	"github.com/aholovko/stave/internal/pkgsnap"
)

// editorFixture is an authored package file with one finding of each exact class. The comment before the P2 literal
// holds é and 😀, so byte, UTF-16 and UTF-32 columns differ on that line.
const editorFixture = `// Package demo is the authored fixture of the STAVE editor integration test.
package demo

import "errors"

// ErrLimit is returned past the limit.
var ErrLimit = /* é😀 */ errors.New("Limit.") // kept

// DemoName returns the naïve 😀 name.
func DemoName(short bool) string {
	if short {
		return "demo"
	} else {
		return "demonstration"
	}
}

// Within reports whether n stays within the limit.
func Within(n_max int) bool {
	return n_max <= 10
}
`

type incoming struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

type published struct {
	URI         string `json:"uri"`
	Version     *int   `json:"version"`
	Diagnostics []struct {
		Range    lspRange `json:"range"`
		Severity int      `json:"severity"`
		Code     string   `json:"code"`
		Source   string   `json:"source"`
		Message  string   `json:"message"`
	} `json:"diagnostics"`
}

// Use real protocol pipes so stray stdout output fails the test.
type lspClient struct {
	t       *testing.T
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  bytes.Buffer
	msgs    chan incoming
	readErr chan error
	exited  chan int
	pending []incoming
	nextID  int
}

func startLSP(t *testing.T, args ...string) *lspClient {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"lsp", "--stdio"}, args...)...)
	cmd.Env = append(os.Environ(), "STAVE_TEST_MAIN=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	c := &lspClient{t: t, cmd: cmd, stdin: stdin, msgs: make(chan incoming, 1000), readErr: make(chan error, 1), exited: make(chan int, 1)}
	cmd.Stderr = &c.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		c.readErr <- c.read(bufio.NewReader(stdout))
		close(c.msgs)
		_ = cmd.Wait()
		c.exited <- cmd.ProcessState.ExitCode()
	}()
	t.Cleanup(func() {
		select {
		case <-c.exited:
		default:
			_ = cmd.Process.Kill()
			<-c.exited
		}
		if strings.Contains(c.stderr.String(), "panic") {
			t.Errorf("server stderr:\n%s", c.stderr.String())
		}
	})
	return c
}

// Run in-process when a test must change configuration between saves.
func serveInProcess(t *testing.T, configure func() harness.Config) *lspClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &lspClient{t: t, stdin: inW, msgs: make(chan incoming, 1000), readErr: make(chan error, 1), exited: make(chan int, 1)}
	code := make(chan int, 1)
	go func() {
		code <- lsp.Serve(configure, inR, outW, &c.stderr)
		outW.Close()
	}()
	go func() {
		c.readErr <- c.read(bufio.NewReader(outR))
		close(c.msgs)
		c.exited <- <-code
	}()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-c.exited:
		case <-time.After(20 * time.Second):
			t.Error("the in-process server did not end after its input closed")
		}
	})
	return c
}

func (c *lspClient) read(r *bufio.Reader) error {
	for {
		length := -1
		for {
			line, err := r.ReadString('\n')
			if err == io.EOF && line == "" && length < 0 {
				return nil
			}
			if err != nil {
				return fmt.Errorf("reading a header: %w (%q)", err, line)
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			name, value, ok := strings.Cut(line, ":")
			if !ok {
				return fmt.Errorf("stdout carried a non-header line %q", line)
			}
			if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
				if length, err = strconv.Atoi(strings.TrimSpace(value)); err != nil {
					return err
				}
			}
		}
		if length < 0 {
			return fmt.Errorf("a frame without Content-Length")
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(r, body); err != nil {
			return fmt.Errorf("reading a %d byte body: %w", length, err)
		}
		var msg incoming
		if err := json.Unmarshal(body, &msg); err != nil {
			return fmt.Errorf("a body that is not JSON-RPC: %w: %s", err, body)
		}
		c.msgs <- msg
	}
}

func (c *lspClient) send(msg map[string]any) {
	c.t.Helper()
	msg["jsonrpc"] = "2.0"
	body, err := json.Marshal(msg)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		c.t.Fatalf("writing %s: %v", msg["method"], err)
	}
}

func (c *lspClient) notify(method string, params any) {
	c.t.Helper()
	c.send(map[string]any{"method": method, "params": params})
}

// Keep notifications received during a request for later assertions.
func (c *lspClient) request(method string, params any) incoming {
	c.t.Helper()
	c.nextID++
	id := c.nextID
	c.send(map[string]any{"id": id, "method": method, "params": params})
	deadline := time.After(20 * time.Second)
	for {
		select {
		case msg, ok := <-c.msgs:
			if !ok {
				c.t.Fatalf("the server ended before answering %s: %v\nstderr:\n%s", method, <-c.readErr, c.stderr.String())
			}
			if len(msg.ID) > 0 && msg.Method == "" && string(msg.ID) == strconv.Itoa(id) {
				return msg
			}
			c.pending = append(c.pending, msg)
		case <-deadline:
			c.t.Fatalf("no answer to %s within 20s", method)
		}
	}
}

// Retain intermediate messages so tests can also assert that stale results were never published.
func (c *lspClient) until(what string, pred func(incoming) bool) []incoming {
	c.t.Helper()
	var seen []incoming
	take := func(msg incoming) bool {
		seen = append(seen, msg)
		return pred(msg)
	}
	for i, msg := range c.pending {
		if take(msg) {
			c.pending = c.pending[i+1:]
			return seen
		}
	}
	c.pending = nil
	deadline := time.After(20 * time.Second)
	for {
		select {
		case msg, ok := <-c.msgs:
			if !ok {
				c.t.Fatalf("the server ended before %s: %v\nstderr:\n%s", what, <-c.readErr, c.stderr.String())
			}
			if take(msg) {
				return seen
			}
		case <-deadline:
			c.t.Fatalf("no %s within 20s; saw %s", what, describe(seen))
		}
	}
}

func (c *lspClient) publish(uri, what string, pred func(published) bool) (published, []incoming) {
	c.t.Helper()
	var got published
	seen := c.until(what, func(msg incoming) bool {
		if msg.Method != "textDocument/publishDiagnostics" {
			return false
		}
		var p published
		if err := json.Unmarshal(msg.Params, &p); err != nil || p.URI != uri {
			return false
		}
		got = p
		return pred(p)
	})
	return got, seen
}

type statusMsg struct {
	URI     string   `json:"uri"`
	Version int      `json:"version"`
	State   string   `json:"state"`
	Summary string   `json:"summary"`
	Detail  []string `json:"detail"`

	SourceSHA256  string `json:"source_sha256"`
	ContextDigest string `json:"context_digest"`
}

func (c *lspClient) status(uri, what string, pred func(statusMsg) bool) (statusMsg, []incoming) {
	c.t.Helper()
	var got statusMsg
	seen := c.until(what, func(msg incoming) bool {
		if msg.Method != "stave/status" {
			return false
		}
		var p statusMsg
		if err := json.Unmarshal(msg.Params, &p); err != nil || p.URI != uri {
			return false
		}
		got = p
		return pred(p)
	})
	return got, seen
}

func statusFor(version int, state string) func(statusMsg) bool {
	return func(st statusMsg) bool { return st.Version == version && (state == "" || st.State == state) }
}

func nonEmptyPublications(msgs []incoming) []string {
	var out []string
	for _, msg := range msgs {
		var p published
		if msg.Method == "textDocument/publishDiagnostics" && json.Unmarshal(msg.Params, &p) == nil && len(p.Diagnostics) != 0 {
			out = append(out, findings(p))
		}
	}
	return out
}

func (c *lspClient) logged(text string) []incoming {
	c.t.Helper()
	return c.until("log message containing "+strconv.Quote(text), func(msg incoming) bool {
		var p struct{ Message string }
		return msg.Method == "window/logMessage" && json.Unmarshal(msg.Params, &p) == nil && strings.Contains(p.Message, text)
	})
}

func (c *lspClient) stop() int {
	c.t.Helper()
	c.request("shutdown", nil)
	c.notify("exit", nil)
	return c.wait()
}

func (c *lspClient) wait() int {
	c.t.Helper()
	select {
	case code := <-c.exited:
		if err := <-c.readErr; err != nil {
			c.t.Errorf("stdout was not protocol only: %v", err)
		}
		c.exited <- code
		return code
	case <-time.After(20 * time.Second):
		c.t.Fatal("the server did not exit within 20s")
		return -1
	}
}

func describe(msgs []incoming) string {
	var out []string
	for _, m := range msgs {
		if m.Method != "" {
			out = append(out, m.Method+" "+string(m.Params))
		} else {
			out = append(out, "response "+string(m.ID))
		}
	}
	return strings.Join(out, "\n")
}

func (c *lspClient) initialize(encodings []string) string {
	c.t.Helper()
	caps := map[string]any{}
	if encodings != nil {
		caps["general"] = map[string]any{"positionEncodings": encodings}
	}
	resp := c.request("initialize", map[string]any{"processId": nil, "rootUri": nil, "capabilities": caps})
	var result struct {
		Capabilities struct {
			PositionEncoding   string `json:"positionEncoding"`
			TextDocumentSync   any    `json:"textDocumentSync"`
			CodeActionProvider any    `json:"codeActionProvider"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil || resp.Error != nil {
		c.t.Fatalf("initialize: %s %v", resp.Result, resp.Error)
	}
	if result.Capabilities.TextDocumentSync == nil || result.Capabilities.CodeActionProvider != nil {
		c.t.Errorf("capabilities = %s, want document sync and no code actions", resp.Result)
	}
	if result.ServerInfo.Name != "stave" || result.ServerInfo.Version != app.Version {
		c.t.Errorf("serverInfo = %+v, want stave %s", result.ServerInfo, app.Version)
	}
	c.notify("initialized", map[string]any{})
	return result.Capabilities.PositionEncoding
}

func (c *lspClient) open(uri, text string, version int) {
	c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": uri, "languageId": "go", "version": version, "text": text,
	}})
}

func (c *lspClient) change(uri, text string, version int) {
	c.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": version},
		"contentChanges": []map[string]any{{"text": text}},
	})
}

func (c *lspClient) save(uri string) {
	c.notify("textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": uri}})
}

func (c *lspClient) close(uri string) {
	c.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": uri}})
}

func fileURI(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// Match editor URI escaping, including the slash before a Windows drive letter.
func TestFileURIRoundTripsTheHostPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a #1 %41", "demo.go")
	u, err := url.Parse(fileURI(path))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.ToSlash(path)
	if !strings.HasPrefix(want, "/") {
		want = "/" + want
	}
	if u.Scheme != "file" || u.Host != "" || u.Path != want {
		t.Errorf("fileURI(%q) = %q, which names %q", path, fileURI(path), u.Path)
	}
}

// site locates a token by a unique needle, byte offset and length.
type site struct {
	code, needle string
	skip, length int
}

var (
	p1     = site{"P1", "} else {", len("} "), len("else")}
	p2     = site{"P2", `"Limit."`, 0, 0}
	p3     = site{"P3", "(n_max int)", len("("), len("n_max")}
	header = site{"DC", "// Package demo is the authored fixture of the STAVE editor integration test.", 0, 0}
)

var exactSites = []site{p1, p2, p3}

var learned = map[string]bool{"TF": true, "DC": true, "RP": true}

// Compute expected positions independently from the server's conversion.
func expectRange(t *testing.T, text string, s site, enc string) lspRange {
	t.Helper()
	at := strings.Index(text, s.needle)
	if at < 0 || strings.Contains(text[at+1:], s.needle) {
		t.Fatalf("%q must occur exactly once", s.needle)
	}
	length := s.length
	if length == 0 {
		length = len(s.needle)
	}
	start := at + s.skip
	lineStart := strings.LastIndex(text[:start], "\n") + 1
	line := strings.Count(text[:start], "\n")
	units := func(str string) int {
		switch enc {
		case "utf-8":
			return len(str)
		case "utf-32":
			return len([]rune(str))
		}
		return len(utf16.Encode([]rune(str)))
	}
	return lspRange{
		Start: lspPosition{Line: line, Character: units(text[lineStart:start])},
		End:   lspPosition{Line: line, Character: units(text[lineStart : start+length])},
	}
}

// Sort findings so assertions do not depend on publication order.
func findings(p published) string {
	var items []string
	for _, d := range p.Diagnostics {
		items = append(items, fmt.Sprintf("%s %d %d:%d-%d:%d", d.Code, d.Severity, d.Range.Start.Line, d.Range.Start.Character, d.Range.End.Line, d.Range.End.Character))
	}
	sort.Strings(items)
	return strings.Join(items, "; ")
}

func expectFindings(t *testing.T, text, enc string, sites ...site) string {
	t.Helper()
	var items []string
	for _, s := range sites {
		r := expectRange(t, text, s, enc)
		severity := 2
		if learned[s.code] {
			severity = 3
		}
		items = append(items, fmt.Sprintf("%s %d %d:%d-%d:%d", s.code, severity, r.Start.Line, r.Start.Character, r.End.Line, r.End.Character))
	}
	sort.Strings(items)
	return strings.Join(items, "; ")
}

// Exact and learned findings share codes, so their messages must distinguish the origin.
func expectOrigins(t *testing.T, p published) {
	t.Helper()
	for _, d := range p.Diagnostics {
		exact := d.Severity == 2
		if exact && !strings.HasSuffix(d.Message, " (exact)") || !exact && !strings.Contains(d.Message, " (advisory: ") {
			t.Errorf("%s message %q does not name its origin", d.Code, d.Message)
		}
	}
}

func hasVersion(v int) func(published) bool {
	return func(p published) bool { return p.Version != nil && *p.Version == v }
}

func TestLSPReportsSavedFindingsInTheNegotiatedEncoding(t *testing.T) {
	for _, tc := range []struct {
		offered []string
		want    string
	}{
		{[]string{"utf-8", "utf-16"}, "utf-8"},
		{nil, "utf-16"},
		{[]string{"utf-32"}, "utf-32"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			dir := writePkg(t, map[string]string{"demo.go": editorFixture})
			uri := fileURI(filepath.Join(dir, "demo.go"))
			c := startLSP(t, "-goos", "linux", "-goarch", "amd64")
			if enc := c.initialize(tc.offered); enc != tc.want {
				t.Fatalf("negotiated %q, want %q", enc, tc.want)
			}

			c.open(uri, editorFixture, 1)
			p, _ := c.publish(uri, "diagnostics for version 1", hasVersion(1))

			if got, want := findings(p), expectFindings(t, editorFixture, tc.want, exactSites...); got != want {
				t.Errorf("published %s\nwant      %s", got, want)
			}
			for _, d := range p.Diagnostics {
				if d.Source != "stave" || d.Message == "" {
					t.Errorf("diagnostic %+v lacks its source or message", d)
				}
			}
			expectOrigins(t, p)
			if code := c.stop(); code != 0 {
				t.Errorf("exit code %d after shutdown and exit, want 0", code)
			}
		})
	}
}

func TestLSPClearsWhileDirtyAndReportsTheSavedSourceAgain(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": editorFixture})
	path := filepath.Join(dir, "demo.go")
	uri := fileURI(path)
	c := startLSP(t, "-goos", "linux", "-goarch", "amd64")
	c.initialize(nil)
	c.open(uri, editorFixture, 1)
	c.publish(uri, "diagnostics for version 1", hasVersion(1))

	c.change(uri, "// unsaved\n"+editorFixture, 2)
	if p, _ := c.publish(uri, "diagnostics for version 2", hasVersion(2)); len(p.Diagnostics) != 0 {
		t.Errorf("dirty buffer: published %s, want nothing", findings(p))
	}

	// Undoing it makes the buffer the saved source again, without a save.
	c.change(uri, editorFixture, 3)
	if p, _ := c.publish(uri, "diagnostics for version 3", hasVersion(3)); findings(p) != expectFindings(t, editorFixture, "utf-16", exactSites...) {
		t.Errorf("restored buffer: published %s", findings(p))
	}

	edited := strings.Replace(editorFixture, "// kept", "// kept\n// and moved down", 1)
	c.change(uri, edited, 4)
	c.publish(uri, "diagnostics for version 4", hasVersion(4))
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	c.save(uri)
	p, _ := c.publish(uri, "diagnostics after the save", func(p published) bool { return len(p.Diagnostics) > 0 })
	if p.Version == nil || *p.Version != 4 || findings(p) != expectFindings(t, edited, "utf-16", exactSites...) {
		t.Errorf("after the save: version %v, published %s", p.Version, findings(p))
	}
	if p.Version != nil && *p.Version != 4 {
		t.Errorf("after the save: version %d, want 4", *p.Version)
	}
	c.stop()
}

func TestLSPReanalysesOpenDocumentsWhenSiblingsChange(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": editorFixture})
	uri := fileURI(filepath.Join(dir, "demo.go"))
	sibling := filepath.Join(dir, "shadow.go")
	c := startLSP(t, "-goos", "linux", "-goarch", "amd64")
	c.initialize(nil)
	c.open(uri, editorFixture, 1)
	c.publish(uri, "diagnostics for version 1", hasVersion(1))
	watched := func(kind int) {
		c.notify("workspace/didChangeWatchedFiles", map[string]any{"changes": []map[string]any{{"uri": fileURI(sibling), "type": kind}}})
	}
	withoutP2 := expectFindings(t, editorFixture, "utf-16", p1, p3)
	withP2 := expectFindings(t, editorFixture, "utf-16", exactSites...)

	if err := os.WriteFile(sibling, []byte("package demo\n\nfunc errors() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	watched(1) // created
	if p, _ := c.publish(uri, "diagnostics after the sibling appeared", func(p published) bool { return findings(p) != withP2 }); findings(p) != withoutP2 {
		t.Errorf("after the sibling appeared: %s, want %s", findings(p), withoutP2)
	}

	if err := os.WriteFile(sibling, []byte("package demo\n\nvar harmless = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	watched(2) // changed
	if p, _ := c.publish(uri, "diagnostics after the sibling changed", func(p published) bool { return findings(p) != withoutP2 }); findings(p) != withP2 {
		t.Errorf("after the sibling changed: %s, want %s", findings(p), withP2)
	}

	if err := os.Remove(sibling); err != nil {
		t.Fatal(err)
	}
	watched(3) // deleted; the evidence changed again, so the document is published again
	if p, _ := c.publish(uri, "diagnostics after the sibling was removed", hasVersion(1)); findings(p) != withP2 {
		t.Errorf("after the removal: %s, want %s", findings(p), withP2)
	}
	c.stop()
}

func TestLSPDropsAnAdvisoryResultForARevisionThatMovedOn(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": editorFixture})
	path := filepath.Join(dir, "demo.go")
	uri := fileURI(path)
	gates := t.TempDir()
	c := startLSP(t, "-goos", "linux", "-goarch", "amd64", "-advisory", "command", "-advisory-cmd", helperArgv(t, "gated", gates))
	c.initialize(nil)

	c.open(uri, editorFixture, 1)
	c.publish(uri, "exact diagnostics for version 1", hasVersion(1))
	waitFor(t, filepath.Join(gates, shaOf(editorFixture)+".waiting"))

	// Supersede the first exchange while it is still blocked.
	edited := strings.Replace(editorFixture, "// kept", "// kept\n// and moved down", 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	c.change(uri, edited, 2)
	c.save(uri)
	c.publish(uri, "exact diagnostics for version 2", hasVersion(2))

	// Release the stale response first to expose any premature diagnostic or completion publication.
	release(t, filepath.Join(gates, shaOf(editorFixture)))
	seen := c.logged("dropped")
	for _, msg := range seen {
		var p published
		if msg.Method == "textDocument/publishDiagnostics" && json.Unmarshal(msg.Params, &p) == nil && strings.Contains(findings(p), "DC") {
			t.Errorf("a stale advisory result was published: %s", findings(p))
		}
		var st statusMsg
		if msg.Method == "stave/status" && json.Unmarshal(msg.Params, &st) == nil && st.Version == 1 && st.State == "complete" {
			t.Errorf("a stale advisory result was stated as complete: %+v", st)
		}
	}

	release(t, filepath.Join(gates, shaOf(edited)))
	p, _ := c.publish(uri, "advisory diagnostics for version 2", func(p published) bool { return strings.Contains(findings(p), "DC") })
	want := expectFindings(t, edited, "utf-16", header, p1, p2, p3)
	if p.Version == nil || *p.Version != 2 || findings(p) != want {
		t.Errorf("version %v, published %s\nwant %s", p.Version, findings(p), want)
	}
	expectOrigins(t, p)
	if st, _ := c.status(uri, "complete status for version 2", statusFor(2, "complete")); !strings.Contains(st.Summary, "4 findings; DC: 1 candidate reported, 1 advised") {
		t.Errorf("status = %+v, want the findings and the candidate count of version 2", st)
	}
	c.stop()
}

// Reopened documents may repeat both URI and version; results must also bind the open instance.
func TestLSPDropsAnInFlightResultOfAClosedDocumentInstance(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": editorFixture})
	uri := fileURI(filepath.Join(dir, "demo.go"))
	gates := t.TempDir()
	c := startLSP(t, "-goos", "linux", "-goarch", "amd64", "-advisory", "command", "-advisory-cmd", helperArgv(t, "gated", gates))
	c.initialize(nil)
	c.open(uri, editorFixture, 1)
	c.publish(uri, "exact diagnostics for version 1", hasVersion(1))
	waitFor(t, filepath.Join(gates, shaOf(editorFixture)+".waiting"))

	// Keep the old exchange in flight across close and reopen.
	c.close(uri)
	c.open(uri, editorFixture, 1)
	c.publish(uri, "exact diagnostics for the reopened document", hasVersion(1))
	if st, _ := c.status(uri, "running status for the reopened document", statusFor(1, "")); st.State != "running" {
		t.Errorf("reopened document: status = %+v, want running", st)
	}

	release(t, filepath.Join(gates, shaOf(editorFixture)))
	seen := c.logged("dropped")
	if got := nonEmptyPublications(seen); len(got) != 0 {
		t.Errorf("the closed instance's result was published for the reopened document: %v", got)
	}
	for _, msg := range seen {
		var st statusMsg
		if msg.Method == "stave/status" && json.Unmarshal(msg.Params, &st) == nil && st.State == "complete" {
			t.Errorf("the closed instance's result was stated as complete: %+v", st)
		}
	}

	p, _ := c.publish(uri, "advisory diagnostics for the reopened document", func(p published) bool { return strings.Contains(findings(p), "DC") })
	if want := expectFindings(t, editorFixture, "utf-16", header, p1, p2, p3); p.Version == nil || *p.Version != 1 || findings(p) != want {
		t.Errorf("version %v, published %s\nwant %s", p.Version, findings(p), want)
	}
	c.status(uri, "complete status for the reopened document", statusFor(1, "complete"))
	if calls := backendCalls(t, gates); len(calls) != 2 {
		t.Errorf("backend starts = %v, want one per document instance", calls)
	}
	c.stop()
}

func backendCalls(t *testing.T, gates string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(gates, "calls"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(body)), "\n")
}

func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// Undo creates a new version of identical text, so the stale exchange cannot supply its advice.
func TestLSPAdvisesTheCurrentVersionAfterASameTextVersionBump(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": editorFixture})
	uri := fileURI(filepath.Join(dir, "demo.go"))
	gates := t.TempDir()
	c := startLSP(t, "-goos", "linux", "-goarch", "amd64", "-advisory", "command", "-advisory-cmd", helperArgv(t, "gated", gates))
	c.initialize(nil)
	c.open(uri, editorFixture, 1)
	c.publish(uri, "exact diagnostics for version 1", hasVersion(1))
	waitFor(t, filepath.Join(gates, shaOf(editorFixture)+".waiting"))

	c.change(uri, editorFixture, 2)
	c.publish(uri, "exact diagnostics for version 2", hasVersion(2))

	release(t, filepath.Join(gates, shaOf(editorFixture)))
	p, _ := c.publish(uri, "advisory diagnostics for version 2", func(p published) bool { return strings.Contains(findings(p), "DC") })
	if p.Version == nil || *p.Version != 2 {
		t.Errorf("advice was published for version %v, want the current version 2", p.Version)
	}
	if want := expectFindings(t, editorFixture, "utf-16", header, p1, p2, p3); findings(p) != want {
		t.Errorf("published %s\nwant      %s", findings(p), want)
	}
	c.stop()
}

func TestLSPShutdownEndsItsAdvisoryBackendAndStartsNoOther(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("alive probes a process with signal 0, which Windows does not support")
	}
	dir := writePkg(t, map[string]string{"demo.go": editorFixture})
	uri := fileURI(filepath.Join(dir, "demo.go"))
	gates := t.TempDir()
	c := startLSP(t, "-goos", "linux", "-goarch", "amd64", "-advisory", "command", "-advisory-cmd", helperArgv(t, "gated", gates))
	c.initialize(nil)
	c.open(uri, editorFixture, 1)
	waitFor(t, filepath.Join(gates, shaOf(editorFixture)+".waiting"))
	calls := backendCalls(t, gates)
	if len(calls) != 1 {
		t.Fatalf("backend starts = %v, want one waiting at its gate", calls)
	}
	pid, err := strconv.Atoi(strings.Fields(calls[0])[0])
	if err != nil || !alive(pid) {
		t.Fatalf("backend pid %q is not a live process (%v)", calls[0], err)
	}
	// The following shutdown response acts as a barrier after this notification.
	c.change(uri, editorFixture, 2)

	if code := c.stop(); code != 0 {
		t.Errorf("exit code %d, want 0", code)
	}
	if alive(pid) {
		t.Errorf("backend %d is still alive after the server exited", pid)
	}
	if calls := backendCalls(t, gates); len(calls) != 1 {
		t.Errorf("backend starts after shutdown = %v, want the queued exchange never to start one", calls)
	}
}

// Shutdown must also end descendants, such as selectors spawned by the runtime.
func TestLSPShutdownEndsTheWorkerBehindALauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no process group on Windows: shutdown ends the direct child only")
	}
	dir := writePkg(t, map[string]string{"demo.go": editorFixture})
	uri := fileURI(filepath.Join(dir, "demo.go"))
	gates := t.TempDir()
	c := startLSP(t, "-goos", "linux", "-goarch", "amd64", "-advisory", "command", "-advisory-cmd", helperArgv(t, "wrap", "gated", gates))
	c.initialize(nil)
	c.open(uri, editorFixture, 1)
	waitFor(t, filepath.Join(gates, shaOf(editorFixture)+".waiting"))
	calls := backendCalls(t, gates)
	if len(calls) != 1 {
		t.Fatalf("worker starts = %v, want one waiting at its gate", calls)
	}
	pid, err := strconv.Atoi(strings.Fields(calls[0])[0])
	if err != nil || !alive(pid) {
		t.Fatalf("worker pid %q is not a live process (%v)", calls[0], err)
	}
	t.Cleanup(func() {
		if alive(pid) {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Kill()
			}
		}
	})

	if code := c.stop(); code != 0 {
		t.Errorf("exit code %d, want 0", code)
	}
	for deadline := time.Now().Add(5 * time.Second); alive(pid) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if alive(pid) {
		t.Errorf("worker %d behind the launcher is still alive after the server exited", pid)
	}
}

func TestLSPStatesWhatItDoesNotAnalyse(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": editorFixture, "demo_test.go": "package demo\n"})
	uri := fileURI(filepath.Join(dir, "demo_test.go"))
	c := startLSP(t, "-goos", "linux", "-goarch", "amd64", "-tests=false")
	c.initialize(nil)
	c.open(uri, "package demo\n", 1)
	if p, _ := c.publish(uri, "diagnostics for the test file", hasVersion(1)); len(p.Diagnostics) != 0 {
		t.Errorf("published %s for a test file", findings(p))
	}
	if st, _ := c.status(uri, "status for the test file", statusFor(1, "")); st.State != "not-analysed" || !strings.Contains(st.Summary, "not analysed") {
		t.Errorf("status = %+v, want not-analysed with the reason", st)
	}
	c.logged("demo_test.go")
	c.stop()
}

func TestLSPPublishesTFAdviceOnASavedTestFileOnlyWithTests(t *testing.T) {
	const test = "package demo\n\nimport \"testing\"\n\n// é😀\nfunc TestInside(t *testing.T) {\n\tt.Error(\"inside\")\n}\n"
	dir := writePkg(t, map[string]string{"demo.go": editorFixture, "demo_test.go": test})
	uri := fileURI(filepath.Join(dir, "demo_test.go"))
	start := strings.Index(test, `t.Error("inside")`)
	recorded := filepath.Join(t.TempDir(), "recorded.json")
	recording := fmt.Sprintf(`{"schema":"stave-advisory-v1","backend":{"id":"six-fixture","digest":"0","detail":"authored"},`+
		`"classes":["P1","P2","P3","TF","DC","RP"],"files":[{"sha256":%q,"status":"checked","reason":null,"classes":["TF"]}],`+
		`"predictions":[{"class":"TF","sha256":%q,"span":{"start":%d,"end":%d},"coverage":"checked","advice":true,"score":1.5,"reason":null}]}`,
		shaOf(test), shaOf(test), start, start+len(`t.Error("inside")`))
	if err := os.WriteFile(recorded, []byte(recording), 0o644); err != nil {
		t.Fatal(err)
	}
	c := startLSP(t, "-goos", "linux", "-goarch", "amd64", "-tests", "-advisory", "recorded", "-advisory-recorded", recorded)
	c.initialize(nil)
	c.open(uri, test, 1)

	p, _ := c.publish(uri, "the TF finding", func(p published) bool {
		return hasVersion(1)(p) && strings.Contains(findings(p), "TF")
	})
	done, _ := c.status(uri, "the final status for version 1", statusFor(1, "complete"))
	if !strings.Contains(done.Summary, "1 finding") || !strings.Contains(done.Summary, "TF: 1 candidate reported, 1 advised") ||
		!strings.Contains(done.Summary, "P1 P2 P3 exact not judged") {
		t.Errorf("summary %q, want the TF outcome and the unjudged exact classes", done.Summary)
	}
	var tf []string
	for _, d := range p.Diagnostics {
		if d.Code == "TF" {
			tf = append(tf, fmt.Sprintf("%d:%d-%d:%d", d.Range.Start.Line, d.Range.Start.Character, d.Range.End.Line, d.Range.End.Character))
		}
	}
	if strings.Join(tf, " ") != "6:1-6:18" {
		t.Errorf("TF findings at %v, want the recorded span at 6:1-6:18", tf)
	}
	c.stop()
}

// An empty diagnostic set still needs explicit completion and zero-candidate coverage.
func TestLSPStatesCompletionAndZeroCandidatesWithoutAnyDiagnostic(t *testing.T) {
	const typesOnly = "// Package demo holds only types.\npackage demo\n\n// Profile is a profile.\ntype Profile struct{ ID string }\n"
	dir := writePkg(t, map[string]string{"demo.go": typesOnly})
	uri := fileURI(filepath.Join(dir, "demo.go"))
	recorded := filepath.Join(t.TempDir(), "recorded.json")
	recording := fmt.Sprintf(`{"schema":"stave-advisory-v1","backend":{"id":"recorded-fixture","digest":"0","detail":"full"},"classes":["DC"],"files":[{"sha256":%q,"status":"checked","reason":null}],"predictions":[]}`, shaOf(typesOnly))
	if err := os.WriteFile(recorded, []byte(recording), 0o644); err != nil {
		t.Fatal(err)
	}
	c := startLSP(t, "-goos", "linux", "-goarch", "amd64", "-advisory", "recorded", "-advisory-recorded", recorded)
	c.initialize(nil)
	c.open(uri, typesOnly, 1)

	running, before := c.status(uri, "the first status for version 1", statusFor(1, ""))
	if running.State != "running" || !strings.Contains(running.Summary, "no findings; advisory running") {
		t.Errorf("first status = %+v, want running", running)
	}
	if detail := strings.Join(running.Detail, "\n"); !strings.Contains(detail, "recorded provider") || strings.Contains(detail, "disabled") {
		t.Errorf("running detail %q must name the configured provider", running.Detail)
	}
	done, after := c.status(uri, "the final status for version 1", statusFor(1, "complete"))
	if !strings.Contains(done.Summary, "no findings; DC: 0 candidates reported") {
		t.Errorf("summary %q lacks the zero-candidate outcome in the shared wording", done.Summary)
	}
	detail := strings.Join(done.Detail, "\n")
	if want := "DC: 0 candidates reported"; !strings.Contains(detail, want) {
		t.Errorf("detail %q lacks %q", done.Detail, want)
	}
	if !strings.Contains(detail, "recorded-fixture") {
		t.Errorf("detail %q does not name the backend", done.Detail)
	}
	if got := nonEmptyPublications(append(before, after...)); len(got) != 0 {
		t.Errorf("a diagnostic was published for a file with no finding: %v", got)
	}
	// Without diagnostics, only the status can bind the result to its source and context.
	code, stdout, stderr := stave(t, "check", "-json", "-goos", "linux", "-goarch", "amd64", dir)
	var rep diag.PackageReport
	if code != 0 || json.Unmarshal([]byte(stdout), &rep) != nil {
		t.Fatalf("check: exit %d, stderr %q", code, stderr)
	}
	if done.SourceSHA256 != shaOf(typesOnly) || done.ContextDigest != rep.Context.Digest || rep.Context.Digest == "" {
		t.Errorf("status identity = %q %q, want the saved source %q under the context %q", done.SourceSHA256, done.ContextDigest, shaOf(typesOnly), rep.Context.Digest)
	}
	c.stop()
}

func TestLSPStatusFollowsTheVersionAndTheSavedSource(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": editorFixture})
	uri := fileURI(filepath.Join(dir, "demo.go"))
	c := startLSP(t, "-goos", "linux", "-goarch", "amd64")
	c.initialize(nil)
	c.open(uri, editorFixture, 1)
	if st, _ := c.status(uri, "status for version 1", statusFor(1, "")); st.State != "complete" || !strings.Contains(st.Summary, "3 findings; advisory disabled") {
		t.Errorf("version 1: %+v, want complete with the findings and the disabled advisory", st)
	}

	c.change(uri, "// unsaved\n"+editorFixture, 2)
	if st, _ := c.status(uri, "status for version 2", statusFor(2, "")); st.State != "unsaved" || !strings.Contains(st.Summary, "buffer differs from the saved file") {
		t.Errorf("dirty buffer: %+v, want unsaved with the byte mismatch stated", st)
	} else if st.SourceSHA256 != "" || st.ContextDigest != "" {
		t.Errorf("dirty buffer: %+v, want no identity: nothing was analysed", st)
	}

	c.change(uri, editorFixture, 3)
	if st, _ := c.status(uri, "status for version 3", statusFor(3, "")); st.State != "complete" || st.SourceSHA256 != shaOf(editorFixture) || st.ContextDigest == "" {
		t.Errorf("restored buffer: %+v, want complete with the saved source's identity", st)
	}
	c.stop()
}

func TestLSPStatesAnAdvisoryFailureWithItsReason(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": editorFixture})
	uri := fileURI(filepath.Join(dir, "demo.go"))
	c := startLSP(t, "-goos", "linux", "-goarch", "amd64", "-advisory", "command", "-advisory-cmd", helperArgv(t, "exit1"))
	c.initialize(nil)
	c.open(uri, editorFixture, 1)

	st, _ := c.status(uri, "the final status for version 1", func(st statusMsg) bool { return st.Version == 1 && st.State != "running" })
	if st.State != "failed" || !strings.Contains(st.Summary, "advisory failed") || !strings.Contains(strings.Join(st.Detail, "\n"), "model artifact is missing") {
		t.Errorf("status = %+v, want failed with the backend's reason", st)
	}
	c.stop()
}

type sequenceProvider struct {
	calls   atomic.Int32
	answers []func(advisory.Request) (advisory.Response, error)
}

func (p *sequenceProvider) Predict(_ context.Context, req advisory.Request) (advisory.Response, error) {
	n := int(p.calls.Add(1))
	if n > len(p.answers) {
		return advisory.Response{}, fmt.Errorf("exchange %d was not expected", n)
	}
	return p.answers[n-1](req)
}

func TestLSPRetriesAFailedAdvisoryExchangeOnAnUnchangedSave(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": editorFixture})
	uri := fileURI(filepath.Join(dir, "demo.go"))
	respond := func(req advisory.Request, rec advisory.FileRecord, predictions ...advisory.Prediction) advisory.Response {
		rec.Path, rec.SHA256 = req.Files[0].Path, req.Files[0].SHA256
		for i := range predictions {
			predictions[i].Path, predictions[i].SHA256 = rec.Path, rec.SHA256
		}
		return advisory.Response{Schema: advisory.Schema, Backend: advisory.Backend{ID: "sequence", Digest: "0", Detail: "authored"},
			Classes: []string{"DC"}, Files: []advisory.FileRecord{rec}, Predictions: predictions}
	}
	reason, yes, score := "selector subprocess timed out", true, 0.9
	start := strings.Index(editorFixture, header.needle)
	provider := &sequenceProvider{answers: []func(advisory.Request) (advisory.Response, error){
		func(advisory.Request) (advisory.Response, error) {
			return advisory.Response{}, fmt.Errorf("backend went away")
		},
		func(req advisory.Request) (advisory.Response, error) {
			return respond(req, advisory.FileRecord{Status: advisory.StatusError, Reason: &reason}), nil
		},
		func(req advisory.Request) (advisory.Response, error) {
			return respond(req, advisory.FileRecord{Status: advisory.StatusChecked, Classes: []string{"DC"}},
				advisory.Prediction{Class: "DC", Span: diag.Span{Start: start, End: start + len(header.needle)},
					Coverage: advisory.StatusChecked, Advice: &yes, Score: &score}), nil
		},
	}}
	c := serveInProcess(t, func() harness.Config {
		return harness.Config{Build: pkgsnap.Build{GOOS: "linux", GOARCH: "amd64"}, Provider: provider, Key: "sequence"}
	})
	c.initialize(nil)
	final := func(st statusMsg) bool { return st.Version == 1 && st.State != "running" }

	c.open(uri, editorFixture, 1)
	if st, _ := c.status(uri, "the status after the failed exchange", final); st.State != "failed" || !strings.Contains(st.Summary, "advisory failed") {
		t.Fatalf("first exchange: %+v, want failed", st)
	}
	c.save(uri)
	if st, _ := c.status(uri, "the status after the class error", final); st.State != "failed" || !strings.Contains(st.Summary, "DC error") {
		t.Fatalf("second exchange: %+v, want failed with DC in error", st)
	}
	c.save(uri)
	if st, _ := c.status(uri, "the status after the checked exchange", final); st.State != "complete" || !strings.Contains(st.Summary, "DC: 1 candidate reported, 1 advised") {
		t.Fatalf("third exchange: %+v, want complete with the advice", st)
	}
	c.save(uri)
	c.request("textDocument/hover", map[string]any{}) // answered only after the save was handled
	for _, msg := range c.pending {
		if msg.Method == "stave/status" {
			t.Errorf("an unchanged save after a successful exchange sent %s %s", msg.Method, msg.Params)
		}
	}
	c.stop()
	if n := provider.calls.Load(); n != 3 {
		t.Errorf("%d exchanges, want 3: a successful exchange is not repeated by an unchanged save", n)
	}
}

// Unjudged exact coverage must not make "no findings" look like a clean result.
func TestLSPNamesTheExactClassesItDidNotJudge(t *testing.T) {
	const source = "package demo\n\nvar Limit = 3\n"
	dir := writePkg(t, map[string]string{"demo.go": source, "broken.go": "package demo\n\nfunc Broken( {\n"})
	uri := fileURI(filepath.Join(dir, "demo.go"))
	c := startLSP(t, "-goos", "linux", "-goarch", "amd64")
	c.initialize(nil)
	c.open(uri, source, 1)

	st, _ := c.status(uri, "status for version 1", statusFor(1, ""))
	if st.State != "complete" {
		t.Errorf("state %q, want complete: a class that was not judged ends the run without failing it", st.State)
	}
	if want := "no findings; P2 exact not judged; advisory disabled"; st.Summary != want {
		t.Errorf("summary %q\nwant    %q", st.Summary, want)
	}
	detail := strings.Join(st.Detail, "\n")
	if want := "P2 exact unsupported: incomplete package context: a sibling's declarations are unknowable [broken.go: parse-error]"; !strings.Contains(detail, want) {
		t.Errorf("detail %q lacks %q", st.Detail, want)
	}
	if want := "P1–P3 exact: no findings"; !strings.Contains(detail, want) {
		t.Errorf("detail %q lacks %q", st.Detail, want)
	}
	c.stop()
}

// Editors can strip a BOM even after save, so a byte mismatch must not be labelled an unsaved edit.
func TestLSPDescribesABufferThatDiffersFromTheSavedFileWithoutNamingACause(t *testing.T) {
	const source = "package demo\n\ntype Profile struct{}\n"
	// The saved file starts with the UTF-8 byte order mark. The editor strips it from the text it sends.
	bom := string([]byte{0xEF, 0xBB, 0xBF})
	dir := writePkg(t, map[string]string{"demo.go": bom + source})
	uri := fileURI(filepath.Join(dir, "demo.go"))
	c := startLSP(t, "-goos", "linux", "-goarch", "amd64")
	c.initialize(nil)
	c.open(uri, source, 1)

	const want = "buffer differs from the saved file (only the saved file is analysed)"
	if st, _ := c.status(uri, "status after the open", statusFor(1, "")); st.State != "unsaved" || st.Summary != want {
		t.Errorf("after the open: %+v\nwant unsaved with the summary %q", st, want)
	}
	c.save(uri)
	if st, _ := c.status(uri, "status after the unchanged save", statusFor(1, "")); st.State != "unsaved" || st.Summary != want {
		t.Errorf("after the save: %+v\nwant unsaved with the summary %q", st, want)
	}
	c.stop()
}

func TestLSPExitCodesAndUnknownRequests(t *testing.T) {
	t.Run("shutdown then exit", func(t *testing.T) {
		c := startLSP(t)
		c.initialize(nil)
		if resp := c.request("textDocument/hover", map[string]any{}); resp.Error == nil || resp.Error.Code != -32601 {
			t.Errorf("unknown request answered %s %v, want MethodNotFound", resp.Result, resp.Error)
		}
		if code := c.stop(); code != 0 {
			t.Errorf("exit code %d, want 0", code)
		}
	})
	t.Run("exit without shutdown", func(t *testing.T) {
		c := startLSP(t)
		c.initialize(nil)
		c.notify("exit", nil)
		if code := c.wait(); code != 1 {
			t.Errorf("exit code %d, want 1", code)
		}
	})
	t.Run("input ends without exit", func(t *testing.T) {
		c := startLSP(t)
		c.initialize(nil)
		c.stdin.Close()
		if code := c.wait(); code != 1 {
			t.Errorf("exit code %d, want 1", code)
		}
	})
}

// An unchanged save must pick up a model switch without repeating a successful run under the same model.
func TestLSPUsesTheConfigurationCurrentAtEachSave(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": editorFixture})
	uri := fileURI(filepath.Join(dir, "demo.go"))
	var key atomic.Value
	key.Store("toy-S")
	configure := func() harness.Config {
		k := key.Load().(string)
		return harness.Config{
			Build:    pkgsnap.Build{GOOS: "linux", GOARCH: "amd64"},
			Provider: advisory.Command{Argv: []string{os.Args[0], "advisory-helper", "echo", k}, Timeout: time.Minute},
			Key:      k,
		}
	}
	names := func(id string) func(statusMsg) bool {
		return func(st statusMsg) bool {
			return st.Version == 1 && st.State == "complete" && strings.Contains(strings.Join(st.Detail, "\n"), "backend "+id+" ")
		}
	}
	c := serveInProcess(t, configure)
	c.initialize(nil)

	c.open(uri, editorFixture, 1)
	c.status(uri, "the complete status under toy-S", names("toy-S"))

	key.Store("toy-B")
	c.save(uri)
	c.status(uri, "the complete status under toy-B after an unchanged save", names("toy-B"))

	c.save(uri)
	c.request("textDocument/hover", map[string]any{}) // answered only after the save was handled
	for _, msg := range c.pending {
		if msg.Method == "textDocument/publishDiagnostics" || msg.Method == "stave/status" {
			t.Errorf("an unchanged save under an unchanged key sent %s %s", msg.Method, msg.Params)
		}
	}
	c.stop()
}

func TestLSPStatesTheConfiguredReasonAdviceIsDisabled(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": editorFixture})
	uri := fileURI(filepath.Join(dir, "demo.go"))
	const reason = "no model installed; run stave setup"
	c := serveInProcess(t, func() harness.Config {
		return harness.Config{Build: pkgsnap.Build{GOOS: "linux", GOARCH: "amd64"}, Disabled: reason, Key: "disabled"}
	})
	c.initialize(nil)
	c.open(uri, editorFixture, 1)

	st, _ := c.status(uri, "status for version 1", statusFor(1, ""))
	if st.State != "complete" || !strings.Contains(st.Summary, "3 findings; advisory disabled") {
		t.Errorf("status = %+v, want complete with the exact findings and the advice disabled", st)
	}
	if want := "advisory classes disabled: " + reason; !slices.Contains(st.Detail, want) {
		t.Errorf("detail %q lacks %q", st.Detail, want)
	}
	c.stop()
}

func waitFor(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
	t.Fatalf("%s never appeared", path)
}

func release(t *testing.T, gate string) {
	t.Helper()
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}
