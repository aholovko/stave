package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/aholovko/stave/internal/app"
	"github.com/aholovko/stave/internal/models"
	"github.com/aholovko/stave/internal/testenv"
)

// home gives the test a data directory of its own and returns it.
func home(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "stave home")
	t.Setenv("STAVE_HOME", dir)
	return dir
}

// toyZip writes a zipped test bundle with a single top-level directory.
func toyZip(t *testing.T, id, variant string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), id+".zip")
	if err := os.WriteFile(path, testenv.Zip(t, testenv.Bundle(t, t.TempDir(), id, variant), id+"/"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	code, stdout, stderr := stave(t, args...)
	if code != 0 {
		t.Fatalf("stave %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, stdout, stderr)
	}
	return stdout
}

// activeRow matches the list row of id when that row is marked active.
func activeRow(id string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(id) + `\s+\S+\s+\S+\s+[0-9a-f]{12}\s+\*\s*$`)
}

func TestSetupThenModelCommandsManageTheActiveBundle(t *testing.T) {
	data := home(t)
	testenv.Install(t)
	t.Setenv("STAVE_TEST_MAIN", "1") // the selector self-test of doctor runs this binary as stave

	out := mustRun(t, "setup", "--no-model")
	if !strings.Contains(out, "runtime ready: "+testenv.VersionLine) || !strings.Contains(out, "no model") {
		t.Errorf("setup --no-model output:\n%s", out)
	}
	if out := mustRun(t, "setup", "--no-model"); !strings.Contains(out, "is set up") {
		t.Errorf("a second setup must reuse the runtime:\n%s", out)
	}

	mustRun(t, "model", "add", toyZip(t, "toy-S", "S"))
	if c, _ := app.LoadConfig(data); c.Model != "toy-S" {
		t.Errorf("the first bundle added is not active: %q", c.Model)
	}
	mustRun(t, "model", "add", testenv.Bundle(t, t.TempDir(), "toy-B", "B"))

	list := mustRun(t, "model", "list")
	if !strings.HasPrefix(list, "ID") || !regexp.MustCompile(`^ID\s+VARIANT\s+VERSION\s+DIGEST\s+ACTIVE\n`).MatchString(list) {
		t.Errorf("list header:\n%s", list)
	}
	if !activeRow("toy-S").MatchString(list) || activeRow("toy-B").MatchString(list) || !strings.Contains(list, "toy-B") {
		t.Errorf("list does not mark toy-S alone as active:\n%s", list)
	}

	mustRun(t, "model", "use", "toy-B")
	if list := mustRun(t, "model", "list"); !activeRow("toy-B").MatchString(list) || activeRow("toy-S").MatchString(list) {
		t.Errorf("use did not switch:\n%s", list)
	}
	if code, _, stderr := stave(t, "model", "remove", "toy-B"); code != 1 || !strings.Contains(stderr, "active") {
		t.Errorf("removing the active bundle: exit %d, stderr %q", code, stderr)
	}
	mustRun(t, "model", "remove", "toy-S")

	doctor := mustRun(t, "doctor")
	for _, want := range []string{"OK uv not needed (runtime set up)", "OK runtime", "OK model toy-B", "OK selector"} {
		if !strings.Contains(doctor, want) {
			t.Errorf("doctor lacks %q:\n%s", want, doctor)
		}
	}
	if strings.Contains(doctor, "FAIL") {
		t.Errorf("doctor fails:\n%s", doctor)
	}

	version := mustRun(t, "version")
	for _, want := range []string{"stave " + app.Version + "\n", "runtime: " + filepath.Join(data, "runtime", app.Version), "model: toy-B (variant B"} {
		if !strings.Contains(version, want) {
			t.Errorf("version lacks %q:\n%s", want, version)
		}
	}
}

// Recover a lost selection without replacing a valid user choice.
func TestSetupRestoresAMissingActiveModelOnly(t *testing.T) {
	data := home(t)
	testenv.Install(t)
	active := func() string {
		t.Helper()
		m, _, err := models.Active(data)
		if err != nil {
			return err.Error()
		}
		return m.ID
	}
	archive := toyZip(t, "toy-S", "S")
	mustRun(t, "setup", "-model", archive)
	if got := active(); got != "toy-S" {
		t.Fatalf("active after the first setup = %q, want toy-S", got)
	}

	for _, source := range []string{"toy-S", archive} {
		if err := os.Remove(filepath.Join(data, "config.json")); err != nil {
			t.Fatal(err)
		}
		if out := mustRun(t, "setup", "-model", source); !strings.Contains(out, "active model: toy-S") {
			t.Errorf("setup -model %s output:\n%s", source, out)
		}
		if got := active(); got != "toy-S" {
			t.Errorf("active after setup -model %s without config.json = %q, want toy-S", source, got)
		}
	}

	mustRun(t, "model", "add", testenv.Bundle(t, t.TempDir(), "toy-B", "B"))
	mustRun(t, "model", "use", "toy-B")
	mustRun(t, "setup", "-model", "toy-S")
	if got := active(); got != "toy-B" {
		t.Errorf("active after setup -model toy-S = %q; a valid selection must stay toy-B", got)
	}
}

// Simulate an unpublished release asset without network access.
type notUploaded struct{ urls []string }

func (n *notUploaded) RoundTrip(req *http.Request) (*http.Response, error) {
	n.urls = append(n.urls, req.URL.String())
	return &http.Response{
		Status:     "404 Not Found",
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

func TestSetupWithTheDefaultModelSaysItIsNotPublishedYet(t *testing.T) {
	home(t)
	testenv.Install(t)
	// The download goes through http.DefaultClient. This client never leaves the process.
	host := &notUploaded{}
	saved := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: host}
	t.Cleanup(func() { http.DefaultClient = saved })

	code, stdout, stderr := stave(t, "setup")
	if code != 1 || !strings.Contains(stdout, "runtime ready") ||
		!strings.Contains(stderr, "S-20261020 is not published yet") ||
		!strings.Contains(stderr, "a local zip or directory can be added instead") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	cat := models.EmbeddedCatalog()
	var want []string
	if url := cat.Bundles[cat.Default].URL; url != "" {
		want = []string{url}
	}
	if !slices.Equal(host.urls, want) {
		t.Errorf("requests %q, want %q from the catalog", host.urls, want)
	}
}

func TestDoctorStatesARemedyForEveryFailure(t *testing.T) {
	home(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("STAVE_TEST_MAIN", "1")
	code, stdout, _ := stave(t, "doctor")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	for _, want := range []string{
		"FAIL uv: uv is not on PATH; install uv",
		"FAIL runtime: runtime not set up; run stave setup",
		"FAIL model: no model installed; run stave setup",
		"OK selector",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("doctor lacks %q:\n%s", want, stdout)
		}
	}
}

// Only stave setup runs uv. Once the runtime is set up, doctor passes without it.
func TestDoctorPassesWithoutUVOnceTheRuntimeIsSetUp(t *testing.T) {
	home(t)
	t.Setenv("STAVE_RUNTIME_PYTHON", testenv.Python(t))
	mustRun(t, "model", "add", toyZip(t, "toy-S", "S"))
	t.Setenv("PATH", t.TempDir())
	t.Setenv("STAVE_TEST_MAIN", "1")
	code, stdout, _ := stave(t, "doctor")
	if code != 0 || !strings.Contains(stdout, "OK uv not needed (runtime set up)\n") || strings.Contains(stdout, "FAIL") {
		t.Errorf("exit %d:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "; loads)") {
		t.Errorf("doctor did not report the successful model load:\n%s", stdout)
	}
}

func TestDoctorDoesNotClaimTheModelLoadsWithoutARuntime(t *testing.T) {
	home(t)
	t.Setenv("STAVE_RUNTIME_PYTHON", testenv.Python(t))
	mustRun(t, "model", "add", toyZip(t, "toy-S", "S"))
	t.Setenv("STAVE_RUNTIME_PYTHON", "")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("STAVE_TEST_MAIN", "1")
	code, stdout, _ := stave(t, "doctor")
	if code != 1 || !strings.Contains(stdout, "FAIL runtime: runtime not set up") ||
		!strings.Contains(stdout, "OK model toy-S") ||
		!strings.Contains(stdout, "; files verified; load not checked)") ||
		strings.Contains(stdout, "; loads)") {
		t.Errorf("exit %d:\n%s", code, stdout)
	}
}

func TestDoctorFailsATamperedActiveBundle(t *testing.T) {
	data := home(t)
	t.Setenv("STAVE_RUNTIME_PYTHON", testenv.Python(t))
	mustRun(t, "model", "add", toyZip(t, "toy-S", "S"))
	if err := os.WriteFile(filepath.Join(data, "models", "toy-S", "model.pt"), []byte("changed weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STAVE_TEST_MAIN", "1")
	code, stdout, _ := stave(t, "doctor")
	if code != 1 || !strings.Contains(stdout, "FAIL model: manifest model.") {
		t.Errorf("exit %d:\n%s", code, stdout)
	}
}

// File hashes alone cannot establish that the runtime still loads the bundle.
func TestDoctorFailsWhenTheActiveBundleDoesNotLoad(t *testing.T) {
	home(t)
	t.Setenv("STAVE_RUNTIME_PYTHON", testenv.Python(t))
	mustRun(t, "model", "add", toyZip(t, "toy-S", "S"))
	t.Setenv("FAKEUV_CHECK_FAIL", "load failed: torch is broken")
	t.Setenv("STAVE_TEST_MAIN", "1")
	code, stdout, _ := stave(t, "doctor")
	if code != 1 || !strings.Contains(stdout, "FAIL model: not compatible with this runtime: stave_infer: load failed: torch is broken; rerun stave setup") {
		t.Errorf("exit %d:\n%s", code, stdout)
	}
}

func TestModelAddRefusedByTheRuntimeLeavesNothing(t *testing.T) {
	data := home(t)
	t.Setenv("STAVE_RUNTIME_PYTHON", testenv.Python(t))
	t.Setenv("FAKEUV_CHECK_FAIL", "load failed: manifest seed: 7 is not 12")
	code, _, stderr := stave(t, "model", "add", toyZip(t, "toy-S", "S"))
	if code != 1 || !strings.Contains(stderr, "stave_infer: load failed: manifest seed: 7 is not 12") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
	if entries, _ := os.ReadDir(filepath.Join(data, "models")); len(entries) != 0 {
		t.Errorf("models/ holds %d entries after a refusal", len(entries))
	}
}

func TestModelAddBeforeSetupSaysToRunSetup(t *testing.T) {
	home(t)
	code, _, stderr := stave(t, "model", "add", toyZip(t, "toy-S", "S"))
	if code != 1 || !strings.Contains(stderr, "run stave setup") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

func TestModelCommandUsage(t *testing.T) {
	home(t)
	for _, args := range [][]string{{"model"}, {"model", "add"}, {"model", "use"}, {"model", "remove", "a", "b"}, {"model", "fetch", "x"}, {"version", "x"}} {
		if code, _, stderr := stave(t, args...); code != 2 || stderr == "" {
			t.Errorf("stave %q: exit %d, stderr %q; want a usage error", args, code, stderr)
		}
	}
	if code, _, stderr := stave(t, "model", "use", "toy-F"); code != 1 || !strings.Contains(stderr, "not installed") {
		t.Errorf("use of an absent model: exit %d, stderr %q", code, stderr)
	}
}

func TestUninstallRemovesTheDataDirectoryAfterConfirmation(t *testing.T) {
	data := home(t)
	t.Setenv("STAVE_RUNTIME_PYTHON", testenv.Python(t))
	mustRun(t, "model", "add", toyZip(t, "toy-S", "S"))
	uninstall := func(answer string, args ...string) (int, string, string) {
		var out, errs bytes.Buffer
		code := run(append([]string{"uninstall"}, args...), strings.NewReader(answer), &out, &errs)
		return code, out.String(), errs.String()
	}

	if code, stdout, _ := uninstall("n\n"); code != 0 || !strings.Contains(stdout, "nothing removed") {
		t.Errorf("declined: exit %d\n%s", code, stdout)
	}
	if _, err := os.Stat(data); err != nil {
		t.Fatalf("a declined uninstall removed the data directory: %v", err)
	}
	code, stdout, _ := uninstall("y\n")
	if code != 0 || !strings.Contains(stdout, "VS Code extension") {
		t.Errorf("confirmed: exit %d\n%s", code, stdout)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Errorf("the data directory survived: %v", err)
	}
	if code, stdout, _ := uninstall("", "--yes"); code != 0 || !strings.Contains(stdout, "nothing to remove") {
		t.Errorf("already removed: exit %d\n%s", code, stdout)
	}
}

func TestUninstallRefusesADirectoryHoldingOtherFiles(t *testing.T) {
	data := home(t)
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "thesis.tex"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := stave(t, "uninstall", "--yes")
	if code != 1 || !strings.Contains(stderr, "thesis.tex") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(data, "thesis.tex")); err != nil {
		t.Errorf("a foreign file was removed: %v", err)
	}
}

func TestUninstallRemovesADirectoryAFileBrowserHasOpened(t *testing.T) {
	data := home(t)
	if err := os.MkdirAll(filepath.Join(data, "models"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".DS_Store", "Thumbs.db"} {
		if err := os.WriteFile(filepath.Join(data, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	code, _, stderr := stave(t, "uninstall", "--yes")
	if code != 0 {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Errorf("the data directory survived: %v", err)
	}
}
