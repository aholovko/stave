package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aholovko/stave/internal/advisory"
	"github.com/aholovko/stave/internal/diag"
	"github.com/aholovko/stave/internal/pkgsnap"
	"github.com/aholovko/stave/internal/testenv"
)

// Reuse this test binary for real CLI and backend subprocesses. Check advisory-helper first because it inherits
// STAVE_TEST_MAIN from the server.
func TestMain(m *testing.M) {
	testenv.Main() // acts as the fake uv or python of the installation tests
	if len(os.Args) >= 3 && os.Args[1] == "advisory-helper" {
		os.Exit(helperBackend(os.Args[2], os.Args[3:]))
	}
	if os.Getenv("STAVE_TEST_MAIN") != "" {
		main()
		return
	}
	// Isolate tests and their child servers from the user's installation.
	data, err := os.MkdirTemp("", "stave-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("STAVE_HOME", data)
	os.Unsetenv("STAVE_RUNTIME_PYTHON")
	code := m.Run()
	os.RemoveAll(data)
	os.Exit(code)
}

// helperArgv returns the -advisory-cmd value that runs this binary as a backend.
func helperArgv(t *testing.T, mode string, args ...string) string {
	t.Helper()
	argv, err := json.Marshal(append([]string{os.Args[0], "advisory-helper", mode}, args...))
	if err != nil {
		t.Fatal(err)
	}
	return string(argv)
}

// helperBackend answers one request. "echo" advises on the first comment of every requested file, under the backend
// id given in args[0], if any. "gated" does the same, but only once a file named after the requested source hash
// appears in args[0], and it reports that it is waiting. The other modes are the ways a backend goes wrong.
func helperBackend(mode string, args []string) int {
	var req advisory.Request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, "helper: bad request:", err)
		return 3
	}
	switch mode {
	case "exit1":
		fmt.Fprintln(os.Stderr, "helper: model artifact is missing")
		return 1
	case "hang":
		// Run long enough to require cancellation.
		if err := os.WriteFile(args[0], []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			return 3
		}
		time.Sleep(60 * time.Second)
		return 0
	case "wrap":
		// Keep a child alive to test cancellation beyond the direct subprocess.
		body, err := json.Marshal(req)
		if err != nil {
			return 3
		}
		child := exec.Command(os.Args[0], append([]string{"advisory-helper"}, args...)...)
		child.Stdin, child.Stdout, child.Stderr = bytes.NewReader(body), os.Stdout, os.Stderr
		if err := child.Run(); err != nil {
			return 3
		}
		return 0
	}
	// Reject duplicate request paths as the real backend does.
	seen := map[string]bool{}
	for i, f := range req.Files {
		if seen[f.Path] {
			fmt.Fprintf(os.Stderr, "helper: files[%d] repeats path %q\n", i, f.Path)
			return 1
		}
		seen[f.Path] = true
	}
	resp := advisory.Response{
		Schema:  advisory.Schema,
		Backend: advisory.Backend{ID: "helper-dc", Digest: "feed", Detail: mode},
		Classes: []string{"DC"},
	}
	if mode == "echo" && len(args) > 0 {
		resp.Backend.ID = args[0] // the backend id a model bundle would give
	}
	yes, score := true, 0.75
	for _, f := range req.Files {
		if mode == "gated" {
			gate := filepath.Join(args[0], f.SHA256)
			if err := os.WriteFile(gate+".waiting", nil, 0o644); err != nil {
				return 3
			}
			// One line per backend start, with the pid that a test can probe.
			calls, err := os.OpenFile(filepath.Join(args[0], "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return 3
			}
			fmt.Fprintf(calls, "%d %s\n", os.Getpid(), f.SHA256)
			calls.Close()
			for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				if _, err := os.Stat(gate); err == nil {
					break
				}
				if time.Now().After(deadline) {
					return 3
				}
			}
		}
		src, err := os.ReadFile(f.Path)
		if err != nil {
			return 3
		}
		rec := advisory.FileRecord{Path: f.Path, SHA256: f.SHA256, Status: advisory.StatusChecked}
		comment := bytes.Index(src, []byte("//"))
		if comment < 0 {
			// A source without a comment has no candidate. It is checked and gets no prediction.
			resp.Files = append(resp.Files, rec)
			continue
		}
		p := advisory.Prediction{Path: f.Path, Class: "DC", SHA256: f.SHA256, Coverage: advisory.StatusChecked, Advice: &yes, Score: &score}
		p.Span.Start = comment
		p.Span.End = p.Span.Start + bytes.IndexByte(src[p.Span.Start:], '\n')
		switch mode {
		case "stale":
			rec.SHA256 = strings.Repeat("0", 64)
			p.SHA256 = rec.SHA256
		case "outofrange":
			p.Span.End = len(src) + 10
		}
		resp.Files = append(resp.Files, rec)
		resp.Predictions = append(resp.Predictions, p)
	}
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		return 3
	}
	return 0
}

func writePkg(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// stave runs the command in-process and returns its exit code and streams.
func stave(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errs bytes.Buffer
	code = run(args, strings.NewReader(""), &out, &errs)
	return code, out.String(), errs.String()
}

// at is the independent position expectation. It returns "line:col" with a byte column, computed from the source text
// alone.
func at(t *testing.T, src, needle string) string {
	t.Helper()
	off := strings.Index(src, needle)
	if off < 0 {
		t.Fatalf("fixture has no %q", needle)
	}
	line := 1 + strings.Count(src[:off], "\n")
	col := off - strings.LastIndex(src[:off], "\n")
	return fmt.Sprintf("%d:%d", line, col)
}

func shaOf(src string) string {
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:])
}

const demoSource = `package demo

import "errors"

var count int = 1

// Café returns the count, obviously.
func Café(ok bool) (int, error) {
	if ok {
		return count, nil
	} else {
		return 0, errors.New("Bad thing.")
	}
}

var snake_case, DemoThing = 1, 2
`

func TestCheckTextPrintsFindingsAtByteColumnsThenCoverage(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": demoSource})
	path := filepath.Join(dir, "demo.go")

	code, stdout, stderr := stave(t, "check", "-goos", "linux", "-goarch", "amd64", dir)

	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{
		path + ":" + at(t, demoSource, "else") + ": P1 exact: ",
		path + ":" + at(t, demoSource, `"Bad thing."`) + ": P2 exact: ",
		path + ":" + at(t, demoSource, "snake_case") + ": P3 exact: ",
		path + ": coverage: exact P1=checked P2=checked P3=checked; " +
			"advisory P1=disabled P2=disabled P3=disabled TF=disabled DC=disabled RP=disabled",
		path + ": not judged: P1 P2 P3 TF DC RP advisory disabled: no model installed; run stave setup",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q\n%s", want, stdout)
		}
	}
	// Explicit default types and package-name repetition are outside the product's served classes.
	// Text output must use current class IDs and contain no advisory findings when advice is disabled.
	if regexp.MustCompile(` [RE][0-9]`).MatchString(stdout) || strings.Contains(stdout, "advisory:") {
		t.Errorf("stdout reports a retired class ID or an advisory finding:\n%s", stdout)
	}
	if strings.Contains(stdout, "candidate") {
		t.Errorf("stdout states a candidate count without a provider:\n%s", stdout)
	}
}

// Shared exact and learned labels must still distinguish their origin.
func TestCheckTextNamesAnAdvisoryFindingByItsClassID(t *testing.T) {
	src := []byte(demoSource)
	snap := &pkgsnap.Snapshot{Files: []pkgsnap.File{{Name: "demo.go", Src: src}}}
	off := strings.Index(demoSource, `"Bad thing."`)
	els := strings.Index(demoSource, "else")
	learned := &diag.Backend{ID: "six"}
	rep := diag.PackageReport{Files: []diag.FileReport{{Name: "demo.go", Role: diag.RoleTarget, Diagnostics: []diag.Diagnostic{
		{Class: "P1", Origin: diag.OriginExact, Span: diag.Span{Start: els}, Message: "m"},
		{Class: "P1", Origin: diag.OriginAdvisory, Span: diag.Span{Start: els}, Message: "m", Backend: learned},
		{Class: "P2", Origin: diag.OriginExact, Span: diag.Span{Start: off}, Message: "m"},
		{Class: "P2", Origin: diag.OriginAdvisory, Span: diag.Span{Start: off}, Message: "m", Backend: learned},
		{Class: "DC", Origin: diag.OriginAdvisory, Span: diag.Span{Start: off}, Message: "m", Backend: learned},
	}}}}

	var out bytes.Buffer
	writeText(&out, group{dir: "pkg"}, snap, rep)

	file := filepath.Join("pkg", "demo.go") + ":"
	pos := file + at(t, demoSource, `"Bad thing."`)
	for _, want := range []string{
		file + at(t, demoSource, "else") + ": P1 exact: m\n", file + at(t, demoSource, "else") + ": P1 advisory: m [six]\n",
		pos + ": P2 exact: m\n", pos + ": P2 advisory: m [six]\n", pos + ": DC advisory: m [six]\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q\n%s", want, out.String())
		}
	}
}

func TestCheckStatesWhatItDidNotJudge(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"demo.go":         demoSource,
		"broken.go":       "package demo\n\nfunc {\n",
		"zz_generated.go": "// Code generated by authored-fixture. DO NOT EDIT.\n\npackage demo\n",
	})

	code, stdout, _ := stave(t, "check", "-goos", "linux", "-goarch", "amd64", dir)

	if code != 0 {
		t.Fatalf("exit %d: an incomplete context is a stated limit, not a failure", code)
	}
	for _, want := range []string{
		filepath.Join(dir, "demo.go") + ": not judged: P2 exact unsupported: incomplete package context",
		"broken.go: parse-error",
		filepath.Join(dir, "zz_generated.go") + ": not analysed (context-only): generated file",
		filepath.Join(dir, "broken.go") + ": not analysed (context-only): parse-error",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, ": P2 exact: ") {
		t.Errorf("P2 was reported under an incomplete context:\n%s", stdout)
	}
}

func TestCheckJSONIsOneReportObjectPerPackage(t *testing.T) {
	one := writePkg(t, map[string]string{"demo.go": demoSource})
	two := writePkg(t, map[string]string{"other.go": "package other\n"})

	code, stdout, stderr := stave(t, "check", "-json", "-goos", "linux", "-goarch", "amd64", "-tags", "a,b", one, two)

	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want one JSON object per package:\n%s", len(lines), stdout)
	}
	var rep diag.PackageReport
	if err := json.Unmarshal([]byte(lines[0]), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Schema != "stave-report-v1" || rep.Build.GOOS != "linux" || strings.Join(rep.Build.Tags, ",") != "a,b" || !rep.Context.Complete {
		t.Errorf("report header = %+v %+v %+v", rep.Schema, rep.Build, rep.Context)
	}
	f := rep.Files[0]
	if f.SHA256 != shaOf(demoSource) || f.Role != "target" || len(f.Coverage) != 9 || len(f.Diagnostics) != 3 {
		t.Errorf("file = %s role %s, %d coverage, %d diagnostics", f.SHA256, f.Role, len(f.Coverage), len(f.Diagnostics))
	}
	// The wire names are the contract, so they are checked as text.
	for _, key := range []string{`"source_sha256":`, `"context_digest":`, `"span":{"start":`, `"origin":"exact"`, `"backend":{"id":"stave-exact"`, `"score":null`, `"incomplete":[]`, `"items":[]`} {
		if !strings.Contains(lines[0], key) {
			t.Errorf("JSON lacks %s\n%s", key, lines[0])
		}
	}
	if strings.Contains(lines[0], `"candidates"`) {
		t.Errorf("JSON carries a candidate count although no provider was configured\n%s", lines[0])
	}
}

// A reported candidate count may be zero. An unserved class retains its unsupported coverage entry but omits the count.
func TestCheckJSONCarriesACountOnlyWhereOneApplies(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": demoSource})
	code, stdout, _ := stave(t, "check", "-json", "-goos", "linux", "-goarch", "amd64", "-advisory", "command", "-advisory-cmd", helperArgv(t, "echo"), dir)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, `"class":"DC","origin":"advisory","status":"checked","reason":"","items":[],"candidates":1}`) {
		t.Errorf("DC coverage lacks its count:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"class":"RP","origin":"advisory","status":"unsupported","reason":"backend helper-dc does not serve this class","items":[]}`) {
		t.Errorf("an unserved class carries a count:\n%s", stdout)
	}
}

func TestCheckingAFileUsesItsSiblingsAsContextOnly(t *testing.T) {
	const src = "package demo\n\nimport \"errors\"\n\nvar ErrBad = errors.New(\"Bad thing.\")\n"
	dir := writePkg(t, map[string]string{
		"demo.go":    src,
		"sibling.go": "package demo\n\nvar other_name int = 2\n",
	})

	code, stdout, _ := stave(t, "check", "-goos", "linux", "-goarch", "amd64", filepath.Join(dir, "demo.go"))
	if code != 0 || !strings.Contains(stdout, "demo.go:"+at(t, src, `"Bad thing."`)+": P2 exact") || strings.Contains(stdout, "sibling.go") {
		t.Errorf("exit %d; want demo.go's finding and nothing about the sibling:\n%s", code, stdout)
	}

	// The sibling is still evidence. Once it shadows errors, the finding goes away.
	if err := os.WriteFile(filepath.Join(dir, "sibling.go"), []byte("package demo\n\nfunc errors() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ = stave(t, "check", "-goos", "linux", "-goarch", "amd64", filepath.Join(dir, "demo.go"))
	if code != 0 || strings.Contains(stdout, "P2 exact: ") {
		t.Errorf("exit %d; a shadowing sibling must remove the finding:\n%s", code, stdout)
	}
}

func TestCheckFailuresGoToStderrWithTheRightExitCode(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": demoSource})
	for name, tc := range map[string]struct {
		args []string
		code int
	}{
		"no command":               {nil, 2},
		"unknown command":          {[]string{"lint"}, 2},
		"no paths":                 {[]string{"check"}, 2},
		"unknown flag":             {[]string{"check", "-nope", dir}, 2},
		"unknown advisory mode":    {[]string{"check", "-advisory", "magic", dir}, 2},
		"recorded without a file":  {[]string{"check", "-advisory", "recorded", dir}, 2},
		"command without an argv":  {[]string{"check", "-advisory", "command", dir}, 2},
		"command argv is not JSON": {[]string{"check", "-advisory", "command", "-advisory-cmd", "python -m x", dir}, 2},
		"a path that is missing":   {[]string{"check", filepath.Join(dir, "absent")}, 1},
		"a file that is missing":   {[]string{"check", filepath.Join(dir, "absent.go")}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := stave(t, tc.args...)
			if code != tc.code || stderr == "" {
				t.Errorf("exit %d, stderr %q; want exit %d with a message", code, stderr, tc.code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want errors on stderr only", stdout)
			}
		})
	}
}

func TestTheConfigurationKeyNamesTheProvider(t *testing.T) {
	for want, o := range map[string]options{
		"disabled":          {advisory: "disabled"},
		"recorded:rec.json": {advisory: "recorded", recorded: "rec.json"},
		"command":           {advisory: "command", command: `["backend"]`},
	} {
		if cfg, err := o.config(); err != nil || cfg.Key != want {
			t.Errorf("%+v: key %q, error %v; want %q", o, cfg.Key, err, want)
		}
	}
}

func TestUsageNamesOnlyTheProductCommands(t *testing.T) {
	code, stdout, _ := stave(t, "help")
	if code != 0 || !strings.Contains(stdout, "stave check") || !strings.Contains(stdout, "stave lsp") || strings.Contains(stdout, "fix") {
		t.Errorf("help: exit %d\n%s\nwant check and lsp, and no fix command", code, stdout)
	}
	if code, _, stderr := stave(t, "fix", "demo.go:1:1"); code != 2 || !strings.Contains(stderr, "unknown command") {
		t.Errorf("fix: exit %d, stderr %q; want the unknown-command usage error", code, stderr)
	}
}

func TestCheckWithACommandBackendEndToEnd(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": demoSource})
	check := func(mode string) (int, string, string) {
		return stave(t, "check", "-goos", "linux", "-goarch", "amd64", "-advisory", "command", "-advisory-cmd", helperArgv(t, mode), dir)
	}
	exactLine := filepath.Join(dir, "demo.go") + ":" + at(t, demoSource, "else") + ": P1 exact: "

	code, stdout, stderr := check("echo")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	advice := filepath.Join(dir, "demo.go") + ":" + at(t, demoSource, "// Café") + ": DC advisory: "
	if !strings.Contains(stdout, advice) || !strings.Contains(stdout, "helper-dc") || !strings.Contains(stdout, "DC=checked") || !strings.Contains(stdout, "RP=unsupported") {
		t.Errorf("stdout lacks the advisory finding, its backend, or its coverage:\n%s", stdout)
	}
	if !strings.Contains(stdout, exactLine) {
		t.Errorf("stdout lacks the exact finding:\n%s", stdout)
	}
	if want := filepath.Join(dir, "demo.go") + ": DC: 1 candidate reported, 1 advised"; !strings.Contains(stdout, want) {
		t.Errorf("stdout lacks the candidate count line %q:\n%s", want, stdout)
	}

	for _, mode := range []string{"exit1", "stale", "outofrange"} {
		code, stdout, stderr := check(mode)
		if code != 1 || !strings.Contains(stderr, "advisory") {
			t.Errorf("%s: exit %d, stderr %q; want exit 1 naming the advisory failure", mode, code, stderr)
		}
		if !strings.Contains(stdout, exactLine) || !strings.Contains(stdout, "DC=error") || strings.Contains(stdout, "DC advisory:") {
			t.Errorf("%s: the exact output must survive and no advice may be reported:\n%s", mode, stdout)
		}
		if strings.Contains(stdout, "candidate") {
			t.Errorf("%s: a count was stated for a failed exchange:\n%s", mode, stdout)
		}
	}
}

// Test files are analysed only with -tests (the default), and then never as context.
func TestCheckAnalysesTestFilesOnlyWithTests(t *testing.T) {
	const test = "package demo_test\n\nimport \"testing\"\n\nfunc TestOutside(t *testing.T) {\n\tt.Error(\"outside\")\n}\n"
	dir := writePkg(t, map[string]string{"demo.go": demoSource, "external_test.go": test})
	path := filepath.Join(dir, "external_test.go")

	_, stdout, _ := stave(t, "check", "-goos", "linux", "-goarch", "amd64", "-tests=false", path)
	if want := path + ": not analysed (ignored): test files lie outside the retained evidence"; !strings.Contains(stdout, want) {
		t.Errorf("with -tests=false stdout lacks %q:\n%s", want, stdout)
	}
	_, before, _ := stave(t, "check", "-json", "-goos", "linux", "-goarch", "amd64", "-tests=false", dir)

	code, stdout, stderr := stave(t, "check", "-goos", "linux", "-goarch", "amd64", "-tests", path)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{
		path + ": coverage: exact P1=unsupported P2=unsupported P3=unsupported; advisory P1=disabled",
		path + ": not judged: P1 P2 P3 exact unsupported: test files lie outside the retained evidence",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("with -tests stdout lacks %q:\n%s", want, stdout)
		}
	}

	_, after, _ := stave(t, "check", "-json", "-goos", "linux", "-goarch", "amd64", "-tests", dir)
	var off, on diag.PackageReport
	if json.Unmarshal([]byte(before), &off) != nil || json.Unmarshal([]byte(after), &on) != nil {
		t.Fatalf("reports do not decode:\n%s\n%s", before, after)
	}
	if on.Context.Digest != off.Context.Digest || on.Context.Digest == "" {
		t.Errorf("context digest %s with -tests, %s without: a test file is never context", on.Context.Digest, off.Context.Digest)
	}
	roles := map[string]string{}
	for _, f := range on.Files {
		roles[f.Name] = f.Role
	}
	if roles["external_test.go"] != "test-target" || roles["demo.go"] != "target" {
		t.Errorf("roles = %v, want the test file as a test target beside the target", roles)
	}
}

func TestCheckWithARecordedBackend(t *testing.T) {
	dir := writePkg(t, map[string]string{"demo.go": demoSource})
	start := strings.Index(demoSource, "// Café")
	recording := fmt.Sprintf(`{"schema":"stave-advisory-v1","backend":{"id":"recorded-fixture","digest":"0","detail":"authored"},"classes":["DC"],
	  "files":[{"sha256":%q,"status":"checked","reason":null}],
	  "predictions":[{"class":"DC","sha256":%q,"span":{"start":%d,"end":%d},"coverage":"checked","advice":true,"score":0.9,"reason":null}]}`,
		shaOf(demoSource), shaOf(demoSource), start, start+len("// Café returns the count, obviously."))
	recorded := filepath.Join(t.TempDir(), "recorded.json")
	if err := os.WriteFile(recorded, []byte(recording), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := stave(t, "check", "-goos", "linux", "-goarch", "amd64", "-advisory", "recorded", "-advisory-recorded", recorded, dir)

	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if want := ":" + at(t, demoSource, "// Café") + ": DC advisory: "; !strings.Contains(stdout, want) || !strings.Contains(stdout, "recorded-fixture") {
		t.Errorf("stdout lacks %q from recorded-fixture:\n%s", want, stdout)
	}
}
