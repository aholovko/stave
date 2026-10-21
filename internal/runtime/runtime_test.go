package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"

	"github.com/aholovko/stave/internal/app"
	"github.com/aholovko/stave/internal/testenv"
)

func TestMain(m *testing.M) {
	testenv.Main()
	os.Exit(m.Run())
}

func TestDirIsNamedAfterTheBinaryVersion(t *testing.T) {
	if got, want := Dir("data"), filepath.Join("data", "runtime", app.Version); got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
}

func TestPythonPathPerOperatingSystem(t *testing.T) {
	dir := filepath.Join("d", "runtime", "dev")
	for goos, want := range map[string]string{
		"windows": filepath.Join(dir, ".venv", "Scripts", "python.exe"),
		"linux":   filepath.Join(dir, ".venv", "bin", "python"),
		"darwin":  filepath.Join(dir, ".venv", "bin", "python"),
	} {
		if got := pythonIn(dir, goos); got != want {
			t.Errorf("%s: %q, want %q", goos, got, want)
		}
	}
}

func TestSetupWithoutUVSaysToInstallIt(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := Setup(context.Background(), t.TempDir(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "install uv") {
		t.Errorf("Setup error %v; want one saying to install uv", err)
	}
}

func TestSetupWritesThePayloadSyncsAndMarksTheRuntimeComplete(t *testing.T) {
	log := testenv.Install(t)
	data := t.TempDir()
	var out bytes.Buffer
	if err := Setup(context.Background(), data, &out); err != nil {
		t.Fatalf("Setup: %v\n%s", err, out.String())
	}
	dir := Dir(data)
	for _, rel := range []string{
		"pyproject.toml", "uv.lock", ".python-version",
		"src/stave_infer/__init__.py", "src/stave_infer/__main__.py", "src/stave_infer/serving.py",
		"src/stave_infer/variants.py", "src/stave_infer/toy.py",
	} {
		got, err1 := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		want, err2 := os.ReadFile(filepath.Join("..", "..", "infer", filepath.FromSlash(rel)))
		if err1 != nil || err2 != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: written %d bytes (%v), source %d bytes (%v)", rel, len(got), err1, len(want), err2)
		}
	}
	modules, _ := filepath.Glob(filepath.Join(dir, "src", "stave_infer", "*.py"))
	if len(modules) != 13 {
		t.Errorf("%d modules written, want the 13 of stave_infer", len(modules))
	}
	if calls := testenv.Calls(t, log); !slices.Equal(calls, []string{"uv sync --locked --no-dev --no-editable"}) {
		t.Errorf("uv calls = %q", calls)
	}
	py, err := Python(data)
	if err != nil || py != pythonIn(dir, goruntime.GOOS) {
		t.Errorf("Python = %q, %v; want the environment's python", py, err)
	}
	if marker, err := os.ReadFile(filepath.Join(dir, completeMarker)); err != nil || !strings.Contains(string(marker), testenv.VersionLine) {
		t.Errorf("marker = %q, %v; want the runtime's version line", marker, err)
	}
	if !strings.Contains(out.String(), testenv.VersionLine) {
		t.Errorf("output lacks the runtime version:\n%s", out.String())
	}
}

func TestSetupReusesACompleteRuntime(t *testing.T) {
	log := testenv.Install(t)
	data := t.TempDir()
	for range 2 {
		if err := Setup(context.Background(), data, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	if calls := testenv.Calls(t, log); len(calls) != 1 {
		t.Errorf("uv ran %d times, want once: a complete runtime is reused", len(calls))
	}
}

// uv can remove the managed Python without removing this runtime's completion marker.
func TestSetupRebuildsARuntimeWhoseInterpreterDoesNotStart(t *testing.T) {
	log := testenv.Install(t)
	data := t.TempDir()
	if err := Setup(context.Background(), data, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pythonIn(Dir(data), goruntime.GOOS)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Setup(context.Background(), data, &out); err != nil {
		t.Fatalf("Setup: %v\n%s", err, out.String())
	}
	if want := "runtime " + Dir(data) + " does not start ("; !strings.Contains(out.String(), want) || !strings.Contains(out.String(), "); setting it up again\n") {
		t.Errorf("output lacks %q…setting it up again:\n%s", want, out.String())
	}
	if calls := testenv.Calls(t, log); len(calls) != 2 {
		t.Errorf("uv ran %d times, want twice: a runtime that does not start is made again", len(calls))
	}
	if v, err := Version(context.Background(), data); err != nil || v != testenv.VersionLine {
		t.Errorf("Version after the rebuild = %q, %v", v, err)
	}
}

// A cancelled health probe must not destroy a potentially healthy runtime.
func TestSetupInterruptedDuringTheProbeKeepsTheRuntime(t *testing.T) {
	log := testenv.Install(t)
	data := t.TempDir()
	if err := Setup(context.Background(), data, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if err := Setup(ctx, data, &out); err == nil || !strings.Contains(err.Error(), "unchanged") {
		t.Errorf("Setup with a cancelled context: %v; want an error saying the runtime is unchanged", err)
	}
	if out.Len() != 0 {
		t.Errorf("an interrupted probe printed:\n%s", out.String())
	}
	if v, err := Version(context.Background(), data); err != nil || v != testenv.VersionLine {
		t.Errorf("Version after the interrupted setup = %q, %v; want the runtime kept", v, err)
	}
	if calls := testenv.Calls(t, log); len(calls) != 1 {
		t.Errorf("uv ran %d times, want once: nothing was set up again", len(calls))
	}
}

func TestSetupRemovesTheRuntimesOfOtherVersions(t *testing.T) {
	testenv.Install(t)
	data := t.TempDir()
	old := filepath.Join(data, "runtime", "dev-0123abc")
	if err := os.MkdirAll(filepath.Join(old, ".venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, completeMarker), []byte(testenv.VersionLine+"\npayload "+strings.Repeat("0", 64)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(data, "runtime", "note.txt")
	if err := os.WriteFile(note, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Setup(context.Background(), data, &out); err != nil {
		t.Fatalf("Setup: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the runtime of another version survived setup: %v", err)
	}
	if _, err := os.Stat(note); err != nil {
		t.Errorf("a file under runtime/ was removed: %v", err)
	}
	if _, err := Python(data); err != nil {
		t.Errorf("Python after setup: %v", err)
	}
	if !strings.Contains(out.String(), "removed the stale runtime "+old+"\n") {
		t.Errorf("output does not name the removal:\n%s", out.String())
	}

	other := filepath.Join(data, "runtime", "dev-4567def")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Setup(context.Background(), data, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil || strings.Contains(out.String(), "removed") {
		t.Errorf("reusing a complete runtime removed %s (%v):\n%s", other, err, out.String())
	}
}

func TestSetupKeepsTheEnvironmentWhereAUserUVSettingWouldMoveIt(t *testing.T) {
	testenv.Install(t)
	elsewhere := filepath.Join(t.TempDir(), "venv")
	t.Setenv("UV_PROJECT_ENVIRONMENT", elsewhere)
	data := t.TempDir()
	var out bytes.Buffer
	if err := Setup(context.Background(), data, &out); err != nil {
		t.Fatalf("Setup: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(pythonIn(Dir(data), goruntime.GOOS)); err != nil {
		t.Errorf("the environment is not in the runtime directory: %v", err)
	}
	if _, err := os.Stat(elsewhere); !os.IsNotExist(err) {
		t.Errorf("uv sync used the user's UV_PROJECT_ENVIRONMENT: %v", err)
	}
}

func TestSetupRedoesAnInterruptedRuntime(t *testing.T) {
	log := testenv.Install(t)
	data := t.TempDir()
	t.Setenv("FAKEUV_FAIL", "1")
	if err := Setup(context.Background(), data, &bytes.Buffer{}); err == nil {
		t.Fatal("Setup succeeded although uv sync failed")
	}
	if _, err := Python(data); !errors.Is(err, ErrNotSetUp) {
		t.Errorf("Python after an interrupted setup: %v; want ErrNotSetUp", err)
	}
	t.Setenv("FAKEUV_FAIL", "")
	var out bytes.Buffer
	if err := Setup(context.Background(), data, &out); err != nil {
		t.Fatalf("rerun: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(Dir(data), ".venv", "partial")); !os.IsNotExist(err) {
		t.Errorf("the half-written environment survived the rerun: %v", err)
	}
	if !strings.Contains(out.String(), "incomplete") {
		t.Errorf("the rerun does not say the runtime was incomplete:\n%s", out.String())
	}
	if calls := testenv.Calls(t, log); len(calls) != 2 {
		t.Errorf("uv ran %d times, want twice", len(calls))
	}
	if _, err := Python(data); err != nil {
		t.Errorf("Python after the rerun: %v", err)
	}
}

// Unstamped builds share "dev", so version alone cannot identify the installed payload.
func TestSetupRedoesARuntimeMadeFromAnotherPayload(t *testing.T) {
	log := testenv.Install(t)
	data := t.TempDir()
	if err := Setup(context.Background(), data, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(Dir(data), completeMarker)
	if err := os.WriteFile(marker, []byte(testenv.VersionLine+"\npayload "+strings.Repeat("0", 64)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Python(data); !errors.Is(err, ErrNotSetUp) {
		t.Errorf("Python of a stale runtime: %v; want ErrNotSetUp", err)
	}
	if err := Setup(context.Background(), data, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if calls := testenv.Calls(t, log); len(calls) != 2 {
		t.Errorf("uv ran %d times, want twice: the stale runtime is made again", len(calls))
	}
	if _, err := Python(data); err != nil {
		t.Errorf("Python after the rerun: %v", err)
	}
}

func TestStatusStatesTheRuntimeWithoutStartingIt(t *testing.T) {
	data := t.TempDir()
	if got := Status(data); got != "runtime not set up; run stave setup" {
		t.Errorf("before setup: %q", got)
	}
	testenv.Install(t)
	if err := Setup(context.Background(), data, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got, want := Status(data), Dir(data)+" ("+testenv.VersionLine+")"; got != want {
		t.Errorf("after setup: %q, want %q", got, want)
	}
	t.Setenv("STAVE_RUNTIME_PYTHON", "py")
	if got := Status(data); got != "py (STAVE_RUNTIME_PYTHON)" {
		t.Errorf("with the override: %q", got)
	}
}

func TestPythonHonoursTheTestOverride(t *testing.T) {
	t.Setenv("STAVE_RUNTIME_PYTHON", filepath.Join("x", "python"))
	if py, err := Python(t.TempDir()); err != nil || py != filepath.Join("x", "python") {
		t.Errorf("Python = %q, %v", py, err)
	}
}

func TestArgvRunsServeOnTheBundleWithThisExecutableAsSelector(t *testing.T) {
	data := t.TempDir()
	if _, err := Argv(data, "b", "stave", nil); !errors.Is(err, ErrNotSetUp) {
		t.Errorf("Argv without a runtime: %v; want ErrNotSetUp", err)
	}
	t.Setenv("STAVE_RUNTIME_PYTHON", "py")
	argv, err := Argv(data, "bundle dir", "/bin/stave", nil)
	if want := []string{"py", "-I", "-m", "stave_infer", "serve", "--bundle", "bundle dir", "--selector", "/bin/stave"}; err != nil || !slices.Equal(argv, want) {
		t.Errorf("Argv = %q, %v; want %q", argv, err, want)
	}
	three := 3
	argv, _ = Argv(data, "b", "s", &three)
	if !slices.Equal(argv[len(argv)-2:], []string{"--threads", "3"}) {
		t.Errorf("Argv with threads = %q", argv)
	}
}

func TestCheckBundleReportsTheRuntimesVerdict(t *testing.T) {
	data := t.TempDir()
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "manifest.json"), []byte(`{"id":"toy-S","variant":"S"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckBundle(context.Background(), data, bundle); !errors.Is(err, ErrNotSetUp) {
		t.Errorf("CheckBundle without a runtime: %v; want ErrNotSetUp", err)
	}
	t.Setenv("STAVE_RUNTIME_PYTHON", testenv.Python(t))
	if err := CheckBundle(context.Background(), data, bundle); err != nil {
		t.Errorf("CheckBundle of a compatible bundle: %v", err)
	}
	t.Setenv("FAKEUV_CHECK_FAIL", "manifest seed: 7 is not the model's seed 12")
	err := CheckBundle(context.Background(), data, bundle)
	if err == nil || !strings.Contains(err.Error(), "stave_infer: manifest seed: 7 is not the model's seed 12") {
		t.Errorf("CheckBundle of an incompatible bundle: %v; want the runtime's reason", err)
	}
}

// Cancellation provides no evidence of bundle incompatibility.
func TestCheckBundleInterruptedSaysSoNotIncompatible(t *testing.T) {
	t.Setenv("STAVE_RUNTIME_PYTHON", testenv.Python(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := CheckBundle(ctx, t.TempDir(), t.TempDir())
	if !errors.Is(err, ErrInterrupted) || strings.Contains(err.Error(), "compatible") {
		t.Errorf("CheckBundle with a cancelled context: %v; want ErrInterrupted", err)
	}
}
