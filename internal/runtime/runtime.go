// Package runtime installs and runs the embedded inference payload in a versioned uv environment.
package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/aholovko/stave"
	"github.com/aholovko/stave/internal/app"
)

// ErrInterrupted means the caller cancelled the command; it is not a verdict on the runtime or bundle.
var ErrInterrupted = errors.New("interrupted")

// ErrNotSetUp is the state of a runtime directory without a completed setup.
var ErrNotSetUp = errors.New("runtime not set up; run stave setup")

// Written last, the marker distinguishes completed installs and binds them to the payload. The digest also catches
// different unstamped builds sharing the "dev" version.
const completeMarker = "COMPLETE"

var payloadDigest = sync.OnceValue(func() string {
	h := sha256.New()
	_ = fs.WalkDir(stave.Payload, "infer", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := stave.Payload.ReadFile(p)
		fmt.Fprintf(h, "%s %d\n", p, len(data))
		h.Write(data)
		return err
	})
	return hex.EncodeToString(h.Sum(nil))
})

// complete reports whether dir holds a runtime set up from the payload of this binary, and returns its version line.
func complete(dir string) (string, bool) {
	marker, err := os.ReadFile(filepath.Join(dir, completeMarker))
	if err != nil {
		return "", false
	}
	version, rest, _ := strings.Cut(string(marker), "\n")
	return version, rest == "payload "+payloadDigest()+"\n"
}

const uvInstall = "https://docs.astral.sh/uv/getting-started/installation/"

// Dir returns the runtime directory of the version of this binary.
func Dir(dataDir string) string {
	return filepath.Join(dataDir, "runtime", app.Version)
}

// Python returns the installed interpreter or ErrNotSetUp. Tests can override it with STAVE_RUNTIME_PYTHON.
func Python(dataDir string) (string, error) {
	if py := os.Getenv("STAVE_RUNTIME_PYTHON"); py != "" {
		return py, nil
	}
	dir := Dir(dataDir)
	if _, ok := complete(dir); !ok {
		return "", fmt.Errorf("%w (%s is not a complete runtime of this build)", ErrNotSetUp, dir)
	}
	return pythonIn(dir, goruntime.GOOS), nil
}

func pythonIn(dir, goos string) string {
	if goos == "windows" {
		return filepath.Join(dir, ".venv", "Scripts", "python.exe")
	}
	return filepath.Join(dir, ".venv", "bin", "python")
}

// FindUV returns the uv executable on PATH.
func FindUV() (string, error) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		return "", fmt.Errorf("uv is not on PATH; install uv (%s), then rerun stave setup", uvInstall)
	}
	return uv, nil
}

// Setup reuses a working runtime or rebuilds it from the locked payload.
// A completed rebuild removes other versions.
// Cancelling the health probe preserves the existing runtime because cancellation does not establish failure.
func Setup(ctx context.Context, dataDir string, w io.Writer) error {
	dir := Dir(dataDir)
	broken := false
	if _, ok := complete(dir); ok {
		// The interpreter of the environment links to a Python that uv manages, which can be removed underneath it.
		_, err := runPython(ctx, pythonIn(dir, goruntime.GOOS), "version")
		if err == nil {
			fmt.Fprintf(w, "runtime %s is set up\n", dir)
			return nil
		}
		if errors.Is(err, ErrInterrupted) {
			return fmt.Errorf("%w; the runtime is unchanged", err)
		}
		fmt.Fprintf(w, "runtime %s does not start (%v); setting it up again\n", dir, err)
		broken = true
	}
	uv, err := FindUV()
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err == nil {
		if !broken {
			fmt.Fprintf(w, "runtime %s is incomplete or from another build; setting it up again\n", dir)
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	if err := writePayload(dir); err != nil {
		return err
	}
	fmt.Fprintf(w, "setting up the runtime in %s with %s (the first run downloads Python and torch)\n", dir, uv)
	// Use locked production dependencies and copy the package into site-packages, independent of the payload directory.
	sync := exec.CommandContext(ctx, uv, "sync", "--locked", "--no-dev", "--no-editable")
	sync.Dir, sync.Stdout, sync.Stderr = dir, w, w
	sync.Env = append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+filepath.Join(dir, ".venv"))
	if err := sync.Run(); err != nil {
		return fmt.Errorf("uv sync in %s failed: %v; rerun stave setup", dir, err)
	}
	version, err := runPython(ctx, pythonIn(dir, goruntime.GOOS), "version")
	if errors.Is(err, ErrInterrupted) {
		return fmt.Errorf("%w; rerun stave setup", err)
	}
	if err != nil {
		return fmt.Errorf("the runtime in %s does not start: %v; rerun stave setup", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, completeMarker), []byte(version+"\npayload "+payloadDigest()+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(w, "runtime ready: %s\n", version)
	return removeStale(dataDir, w)
}

func removeStale(dataDir string, w io.Writer) error {
	root := filepath.Join(dataDir, "runtime")
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == app.Version {
			continue
		}
		stale := filepath.Join(root, e.Name())
		if err := os.RemoveAll(stale); err != nil {
			return err
		}
		fmt.Fprintf(w, "removed the stale runtime %s\n", stale)
	}
	return nil
}

// writePayload writes the embedded project into dir, without the infer/ prefix it has in the repository.
func writePayload(dir string) error {
	return fs.WalkDir(stave.Payload, "infer", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := stave.Payload.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := strings.CutPrefix(p, "infer/")
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// Status reads the setup receipt without starting the runtime.
func Status(dataDir string) string {
	if py := os.Getenv("STAVE_RUNTIME_PYTHON"); py != "" {
		return py + " (STAVE_RUNTIME_PYTHON)"
	}
	dir := Dir(dataDir)
	version, ok := complete(dir)
	if !ok {
		return ErrNotSetUp.Error()
	}
	return fmt.Sprintf("%s (%s)", dir, version)
}

// Version returns the version line of the runtime itself, which says what it is and what it supports.
func Version(ctx context.Context, dataDir string) (string, error) {
	py, err := Python(dataDir)
	if err != nil {
		return "", err
	}
	return runPython(ctx, py, "version")
}

// CheckBundle verifies compatibility by constructing the model and loading its weights in the runtime.
func CheckBundle(ctx context.Context, dataDir, bundleDir string) error {
	py, err := Python(dataDir)
	if err != nil {
		return err
	}
	if _, err := runPython(ctx, py, "check-bundle", bundleDir); err != nil {
		if errors.Is(err, ErrInterrupted) {
			return err
		}
		return fmt.Errorf("not compatible with this runtime: %v", err)
	}
	return nil
}

// Argv returns the backend command, using this binary as the structural selector.
func Argv(dataDir, bundleDir, selfExe string, threads *int) ([]string, error) {
	py, err := Python(dataDir)
	if err != nil {
		return nil, err
	}
	argv := []string{py, "-I", "-m", "stave_infer", "serve", "--bundle", bundleDir, "--selector", selfExe}
	if threads != nil {
		argv = append(argv, "--threads", strconv.Itoa(*threads))
	}
	return argv, nil
}

// runPython returns the last stdout line. Isolated mode prevents PYTHON* variables and user packages from changing
// the installed runtime's imports.
func runPython(ctx context.Context, py string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, py, append([]string{"-I", "-m", "stave_infer"}, args...)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ErrInterrupted
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", errors.New(lastLine(msg))
		}
		return "", fmt.Errorf("%s -I -m stave_infer %s: %v", path.Base(filepath.ToSlash(py)), args[0], err)
	}
	return lastLine(strings.TrimSpace(stdout.String())), nil
}

func lastLine(s string) string {
	return s[strings.LastIndex(s, "\n")+1:]
}
