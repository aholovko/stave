// Package testenv supplies cross-platform installation fakes and small model bundles.
// Tests call Main from TestMain so copies of their binary can act as uv or Python.
// FAKEUV_LOG records uv calls; FAKEUV_FAIL and FAKEUV_CHECK_FAIL inject sync and bundle-check failures.
package testenv

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// VersionLine is what the fake python prints for `-I -m stave_infer version`.
const VersionLine = "stave-infer 0.1.0 (bundle stave-model-bundle-1; models stave-flat-1 stave-structural-6; protocol stave-advisory-v1)"

// Main acts as the fake uv or python and exits when the running binary was installed as one. Otherwise it returns.
func Main() {
	switch strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") {
	case "uv":
		os.Exit(uv(os.Args[1:]))
	case "python":
		os.Exit(python(os.Args[1:]))
	}
}

// Install puts the fake uv first on PATH for the rest of the test and returns the path of its call log.
func Install(t testing.TB) string {
	t.Helper()
	bin := t.TempDir()
	if err := copyExecutable(filepath.Join(bin, exe("uv"))); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "uv.log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKEUV_LOG", log)
	return log
}

// Python installs the fake python into a fresh directory and returns its path, for a test that points
// STAVE_RUNTIME_PYTHON at it.
func Python(t testing.TB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), exe("python"))
	if err := copyExecutable(path); err != nil {
		t.Fatal(err)
	}
	return path
}

// Calls reads the fake uv's call log: one line per call.
func Calls(t testing.TB, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func uv(args []string) int {
	cwd, _ := os.Getwd()
	if log := os.Getenv("FAKEUV_LOG"); log != "" {
		f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "uv %s\n", strings.Join(args, " "))
			f.Close()
		}
	}
	if !slices.Equal(args, []string{"sync", "--locked", "--no-dev", "--no-editable"}) {
		fmt.Fprintf(os.Stderr, "fake uv: unexpected arguments %q\n", args)
		return 2
	}
	for _, name := range []string{"pyproject.toml", "uv.lock", ".python-version", filepath.Join("src", "stave_infer", "__main__.py")} {
		if _, err := os.Stat(filepath.Join(cwd, name)); err != nil {
			fmt.Fprintf(os.Stderr, "fake uv: the project lacks %s\n", name)
			return 2
		}
	}
	venv := filepath.Join(cwd, ".venv")
	if env := os.Getenv("UV_PROJECT_ENVIRONMENT"); env != "" {
		venv = filepath.Join(cwd, env)
		if filepath.IsAbs(env) {
			venv = env
		}
	}
	if err := os.MkdirAll(venv, 0o755); err != nil {
		return 2
	}
	if os.Getenv("FAKEUV_FAIL") != "" {
		_ = os.WriteFile(filepath.Join(venv, "partial"), nil, 0o644)
		fmt.Fprintln(os.Stderr, "fake uv: interrupted")
		return 1
	}
	bin := filepath.Join(venv, "bin")
	if runtime.GOOS == "windows" {
		bin = filepath.Join(venv, "Scripts")
	}
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return 2
	}
	if err := copyExecutable(filepath.Join(bin, exe("python"))); err != nil {
		fmt.Fprintln(os.Stderr, "fake uv:", err)
		return 2
	}
	fmt.Println("Installed 1 package")
	return 0
}

func python(args []string) int {
	switch {
	case slices.Equal(args, []string{"-I", "-m", "stave_infer", "version"}):
		fmt.Println(VersionLine)
		return 0
	case len(args) == 5 && slices.Equal(args[:4], []string{"-I", "-m", "stave_infer", "check-bundle"}):
		if reason := os.Getenv("FAKEUV_CHECK_FAIL"); reason != "" {
			fmt.Fprintln(os.Stderr, "stave_infer: "+reason)
			return 1
		}
		var m struct {
			ID      string `json:"id"`
			Variant string `json:"variant"`
		}
		data, err := os.ReadFile(filepath.Join(args[4], "manifest.json"))
		if err == nil {
			err = json.Unmarshal(data, &m)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "stave_infer: manifest:", err)
			return 1
		}
		fmt.Printf("%s: compatible (variant %s, fake)\n", m.ID, m.Variant)
		return 0
	}
	fmt.Fprintf(os.Stderr, "fake python: unsupported arguments %q\n", args)
	return 2
}

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func copyExecutable(path string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	in, err := os.Open(self)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
