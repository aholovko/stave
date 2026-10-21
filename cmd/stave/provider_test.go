package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aholovko/stave/internal/advisory"
	"github.com/aholovko/stave/internal/app"
	"github.com/aholovko/stave/internal/diag"
	"github.com/aholovko/stave/internal/harness"
	"github.com/aholovko/stave/internal/models"
	"github.com/aholovko/stave/internal/testenv"
)

// install puts a test bundle into data as models.Add does, without a runtime.
func install(t *testing.T, data, id, variant string) string {
	t.Helper()
	pass := func(string) error { return nil }
	if _, err := models.Add(t.Context(), data, testenv.Bundle(t, t.TempDir(), id, variant), pass, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	return models.Dir(data, id)
}

// resolved returns the configuration that check would analyse with after it parses args.
func resolved(t *testing.T, data string, args ...string) harness.Config {
	t.Helper()
	var o options
	fs := flagSet("check", &strings.Builder{})
	o.buildFlags(fs)
	o.advisoryFlags(fs)
	if code := parse(fs, args); code >= 0 {
		t.Fatalf("parse %q: exit %d", args, code)
	}
	o.visited(fs)
	if _, err := o.config(); err != nil {
		t.Fatal(err)
	}
	return o.configure(data, "/bin/stave")()
}

func TestModelProviderStatesWhyAdviceIsDisabled(t *testing.T) {
	t.Setenv("STAVE_RUNTIME_PYTHON", "")
	data := t.TempDir()
	expect := func(want string, args ...string) {
		t.Helper()
		cfg := resolved(t, data, args...)
		if cfg.Provider != nil || cfg.Disabled != want {
			t.Errorf("%q: provider %v, reason %q; want disabled with %q", args, cfg.Provider, cfg.Disabled, want)
		}
	}
	expect("no model installed; run stave setup")
	expect("model toy-B is not installed; run stave model list", "-model", "toy-B")
	if err := app.SaveConfig(data, app.Config{Model: "S-20261010", AdvisoryTimeout: time.Minute, Tests: true}); err != nil {
		t.Fatal(err)
	}
	expect("model S-20261010 is not installed; run stave model list")
	install(t, data, "toy-S", "S")
	if err := models.Use(data, "toy-S"); err != nil {
		t.Fatal(err)
	}
	expect("runtime not set up; run stave setup")
}

func TestModelProviderRunsTheActiveOrNamedBundle(t *testing.T) {
	data := t.TempDir()
	t.Setenv("STAVE_RUNTIME_PYTHON", "py")
	sDir := install(t, data, "toy-S", "S")
	bDir := install(t, data, "toy-B", "B")

	cfg := resolved(t, data)
	cmd, ok := cfg.Provider.(advisory.Command)
	want := []string{"py", "-I", "-m", "stave_infer", "serve", "--bundle", sDir, "--selector", "/bin/stave"}
	if !ok || !slices.Equal(cmd.Argv, want) || cmd.Timeout != 180*time.Second || cfg.Key != "toy-S" || !cfg.Build.TestTargets {
		t.Errorf("active bundle: %+v key %q tests %v; want %q for 180s under key toy-S with tests", cfg.Provider, cfg.Key, cfg.Build.TestTargets, want)
	}

	cfg = resolved(t, data, "-model", "toy-B", "-advisory-timeout", "5s", "-tests=false")
	cmd, _ = cfg.Provider.(advisory.Command)
	if len(cmd.Argv) < 7 || cmd.Argv[6] != bDir || cmd.Timeout != 5*time.Second || cfg.Key != "toy-B" || cfg.Build.TestTargets {
		t.Errorf("flags: %+v key %q tests %v", cfg.Provider, cfg.Key, cfg.Build.TestTargets)
	}

	three := 3
	if err := app.SaveConfig(data, app.Config{Model: "toy-B", Threads: &three, AdvisoryTimeout: 90 * time.Second, Tests: false}); err != nil {
		t.Fatal(err)
	}
	cfg = resolved(t, data)
	cmd, _ = cfg.Provider.(advisory.Command)
	if !slices.Equal(cmd.Argv[len(cmd.Argv)-2:], []string{"--threads", "3"}) || cmd.Timeout != 90*time.Second || cfg.Key != "toy-B" || cfg.Build.TestTargets {
		t.Errorf("config.json: %+v key %q tests %v", cfg.Provider, cfg.Key, cfg.Build.TestTargets)
	}

	// The other providers take their timeout and test selection from config.json too.
	cfg = resolved(t, data, "-advisory", "command", "-advisory-cmd", `["backend"]`)
	if cmd, _ := cfg.Provider.(advisory.Command); cmd.Timeout != 90*time.Second || cfg.Key != "command" || cfg.Build.TestTargets {
		t.Errorf("command provider: %+v key %q", cfg.Provider, cfg.Key)
	}
}

// The language server asks at every analysis, so a switch applies at the next save without a restart.
func TestTheConfigurationSourceFollowsAModelSwitch(t *testing.T) {
	data := t.TempDir()
	t.Setenv("STAVE_RUNTIME_PYTHON", "py")
	install(t, data, "toy-S", "S")
	install(t, data, "toy-B", "B")
	var o options
	o.advisory = "model"
	configure := o.configure(data, "stave")
	if key := configure().Key; key != "toy-S" {
		t.Fatalf("key %q, want toy-S", key)
	}
	if err := models.Use(data, "toy-B"); err != nil {
		t.Fatal(err)
	}
	if key := configure().Key; key != "toy-B" {
		t.Errorf("key after the switch %q, want toy-B", key)
	}
}

func TestCheckJSONCarriesTheDisabledReason(t *testing.T) {
	home(t)
	dir := writePkg(t, map[string]string{"demo.go": demoSource})
	code, stdout, stderr := stave(t, "check", "-json", dir)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	var rep diag.PackageReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Advisory.Status != diag.StatusDisabled || rep.Advisory.Reason != "no model installed; run stave setup" {
		t.Errorf("advisory = %+v", rep.Advisory)
	}
	if len(rep.Files[0].Diagnostics) != 3 {
		t.Errorf("%d exact findings, want 3: exact analysis runs without a model", len(rep.Files[0].Diagnostics))
	}
}

func TestCheckAnalysesTestFilesAsConfigured(t *testing.T) {
	data := home(t)
	const test = "package demo_test\n\nimport \"testing\"\n\nfunc TestOutside(t *testing.T) {\n\tt.Error(\"outside\")\n}\n"
	dir := writePkg(t, map[string]string{"demo.go": demoSource, "external_test.go": test})
	role := func(args ...string) string {
		t.Helper()
		_, stdout, _ := stave(t, append(append([]string{"check", "-json"}, args...), dir)...)
		var rep diag.PackageReport
		if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
			t.Fatalf("%v\n%s", err, stdout)
		}
		for _, f := range rep.Files {
			if f.Name == "external_test.go" {
				return f.Role
			}
		}
		return ""
	}
	if got := role(); got != diag.RoleTestTarget {
		t.Errorf("default: role %q, want test files analysed (config.json tests: true)", got)
	}
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "config.json"), []byte(`{"tests": false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := role(); got == diag.RoleTestTarget {
		t.Errorf("tests false in config.json: the test file is still a target")
	}
	if got := role("-tests"); got != diag.RoleTestTarget {
		t.Errorf("-tests overrides config.json: role %q", got)
	}
}

func TestCheckStatesAMalformedConfigAsTheReason(t *testing.T) {
	data := home(t)
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "config.json"), []byte(`{"model": `), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := writePkg(t, map[string]string{"demo.go": demoSource})
	code, stdout, _ := stave(t, "check", "-json", dir)
	var rep diag.PackageReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil || code != 0 {
		t.Fatalf("exit %d, %v\n%s", code, err, stdout)
	}
	if rep.Advisory.Status != diag.StatusDisabled || !strings.HasPrefix(rep.Advisory.Reason, filepath.Join(data, "config.json")+": ") {
		t.Errorf("advisory = %+v; want disabled with the unreadable config.json named", rep.Advisory)
	}
}

func TestModelFlagNeedsTheModelProvider(t *testing.T) {
	home(t)
	dir := writePkg(t, map[string]string{"demo.go": demoSource})
	if code, _, stderr := stave(t, "check", "-advisory", "disabled", "-model", "toy-S", dir); code != 2 || !strings.Contains(stderr, "-model") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr := stave(t, "check", "-advisory-cmd", `["x"]`, dir); code != 2 || stderr == "" {
		t.Errorf("-advisory-cmd under the model provider: exit %d, stderr %q", code, stderr)
	}
}

func TestCheckAndLSPRejectNonpositiveAdvisoryTimeouts(t *testing.T) {
	home(t)
	dir := writePkg(t, map[string]string{"demo.go": demoSource})
	for _, command := range []string{"check", "lsp"} {
		for _, timeout := range []string{"0s", "-1s"} {
			t.Run(command+"/"+timeout, func(t *testing.T) {
				args := []string{command, "-advisory", "command", "-advisory-cmd", helperArgv(t, "echo"),
					"-advisory-timeout", timeout}
				if command == "check" {
					args = append(args, dir)
				}
				code, stdout, stderr := stave(t, args...)
				if code != exitUsage || stdout != "" || !strings.Contains(stderr, "-advisory-timeout") ||
					!strings.Contains(stderr, "positive") {
					t.Errorf("exit %d, stdout %q, stderr %q; want a timeout usage error", code, stdout, stderr)
				}
			})
		}
	}
}

// Keep the library and product timeout defaults consistent.
func TestTheExchangeTimeoutDefaultsAgree(t *testing.T) {
	if advisory.DefaultTimeout != app.DefaultConfig().AdvisoryTimeout {
		t.Errorf("advisory.DefaultTimeout = %v, config default = %v", advisory.DefaultTimeout, app.DefaultConfig().AdvisoryTimeout)
	}
}
