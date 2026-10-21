package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aholovko/stave/internal/diag"
)

// Alternate path spellings must not duplicate backend inputs or reports.
func TestCheckMergesOverlappingSpellingsOfOnePackage(t *testing.T) {
	dir := writePkg(t, map[string]string{
		"demo.go":  demoSource,
		"other.go": "package demo\n\nvar other_name = 1\n",
	})
	t.Chdir(dir)
	absDemo := filepath.Join(dir, "demo.go")

	for name, tc := range map[string]struct {
		args  []string
		roles map[string]string
	}{
		"a relative directory and an absolute member": {
			[]string{".", absDemo},
			map[string]string{"demo.go": diag.RoleTarget, "other.go": diag.RoleTarget},
		},
		"an absolute directory and a relative member": {
			[]string{dir, "./demo.go"},
			map[string]string{"demo.go": diag.RoleTarget, "other.go": diag.RoleTarget},
		},
		"one member under two spellings": {
			[]string{"./demo.go", absDemo},
			map[string]string{"demo.go": diag.RoleTarget, "other.go": diag.RoleContextOnly},
		},
	} {
		t.Run(name, func(t *testing.T) {
			args := append([]string{"check", "-json", "-goos", "linux", "-goarch", "amd64", "-advisory", "command", "-advisory-cmd", helperArgv(t, "echo")}, tc.args...)
			code, stdout, stderr := stave(t, args...)

			if code != 0 || stderr != "" {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			lines := strings.Split(strings.TrimSpace(stdout), "\n")
			if len(lines) != 1 {
				t.Fatalf("got %d reports, want one package:\n%s", len(lines), stdout)
			}
			var rep diag.PackageReport
			if err := json.Unmarshal([]byte(lines[0]), &rep); err != nil {
				t.Fatal(err)
			}
			for _, f := range rep.Files {
				if want := tc.roles[f.Name]; f.Role != want {
					t.Errorf("%s: role = %s (%s), want %s", f.Name, f.Role, f.Reason, want)
				}
				if f.Name == "demo.go" && !strings.Contains(found(f), "DC") {
					t.Errorf("demo.go: findings = %s, want the backend's DC advice", found(f))
				}
			}
		})
	}
}

func TestCheckExitsOneWhenAClassCouldNotBeAnalysed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fixture-pkg")
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("..", "..", "testdata", "fixture-pkg"))); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(dir, "demo.go"))
	if err != nil {
		t.Fatal(err)
	}
	recorded := filepath.Join(t.TempDir(), "recorded.json")
	recording := `{"schema":"stave-advisory-v1","backend":{"id":"recorded-fixture","digest":"0","detail":"authored"},"classes":["DC"],` +
		`"files":[{"sha256":"` + shaOf(string(src)) + `","status":"error","reason":"selector subprocess timed out"}],"predictions":[]}`
	if err := os.WriteFile(recorded, []byte(recording), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := stave(t, "check", "-goos", "linux", "-goarch", "amd64", "-advisory", "recorded", "-advisory-recorded", recorded, dir)

	if want := "stave check: " + filepath.Join(dir, "demo.go") + ": DC advisory error: selector subprocess timed out\n"; code != 1 || !strings.Contains(stderr, want) {
		t.Errorf("exit %d, stderr %q; want exit 1 and %q", code, stderr, want)
	}
	if !strings.Contains(stdout, "DC=error") {
		t.Errorf("the report must still state the class in error:\n%s", stdout)
	}

	// After a failed exchange every advisory class is in error for the same reason, which stderr states once.
	code, _, stderr = stave(t, "check", "-goos", "linux", "-goarch", "amd64", "-advisory", "command", "-advisory-cmd", `["`+filepath.ToSlash(filepath.Join(t.TempDir(), "absent-backend"))+`"]`, dir)
	if code != 1 || strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, "advisory provider command failed") {
		t.Errorf("exit %d, stderr %q; want exit 1 and one provider line", code, stderr)
	}
}

// found renders the findings of a file as "CLASS@start-end" items.
func found(f diag.FileReport) string {
	var items []string
	for _, d := range f.Diagnostics {
		items = append(items, d.Class+"@"+strconv.Itoa(d.Span.Start)+"-"+strconv.Itoa(d.Span.End))
	}
	return strings.Join(items, " ")
}

// An interrupted `stave check` ends the backend chain it started, both the launcher and the worker, and says so.
func TestCheckInterruptEndsTheAdvisoryBackendChain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Interrupt cannot be sent to another process on Windows")
	}
	dir := writePkg(t, map[string]string{"demo.go": demoSource})
	pidfile := filepath.Join(t.TempDir(), "worker.pid")
	cmd := exec.Command(os.Args[0], "check", "-goos", "linux", "-goarch", "amd64",
		"-advisory", "command", "-advisory-cmd", helperArgv(t, "wrap", "hang", pidfile), dir)
	cmd.Env = append(os.Environ(), "STAVE_TEST_MAIN=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Share one completion signal between cleanup and the interrupt assertion.
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			_ = cmd.Process.Kill()
			<-exited
		}
	})

	// The worker creates its pid file before it writes the pid, so wait for a pid, not only for the file.
	var pid int
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		body, err := os.ReadFile(pidfile)
		if err == nil {
			if pid, err = strconv.Atoi(strings.TrimSpace(string(body))); err == nil && pid > 0 {
				break
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("pid file never held a pid: last read %q, err %v", body, err)
		}
	}
	t.Cleanup(func() {
		if alive(pid) {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Kill()
			}
		}
	})

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		t.Fatal("stave check did not exit within 15s of the interrupt")
	}
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Errorf("exit code %d, want 1 (an exchange that was cut short is an advisory failure)\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "cancel") {
		t.Errorf("stderr %q does not name the cancellation", stderr.String())
	}
	if !strings.Contains(stdout.String(), "P1 exact") {
		t.Errorf("the exact findings were not reported:\n%s", stdout.String())
	}
	for deadline := time.Now().Add(5 * time.Second); alive(pid) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if alive(pid) {
		t.Errorf("worker %d is still alive after stave check was interrupted", pid)
	}
}
