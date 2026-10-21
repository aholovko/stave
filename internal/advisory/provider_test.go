package advisory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Reuse the test binary as a backend to exercise real subprocess pipes, exit codes and cancellation.
func TestMain(m *testing.M) {
	if mode := os.Getenv("STAVE_ADVISORY_HELPER"); mode != "" {
		os.Exit(helperBackend(mode))
	}
	os.Exit(m.Run())
}

func helperBackend(mode string) int {
	var req Request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, "helper: bad request:", err)
		return 3
	}
	switch mode {
	case "exit1":
		fmt.Fprintln(os.Stderr, "helper: model artifact is missing")
		return 1
	case "garbage":
		fmt.Println("this is not the protocol")
		return 0
	case "hang":
		// Run long enough to require cancellation.
		if pidfile := os.Getenv("STAVE_HELPER_PIDFILE"); pidfile != "" {
			if err := os.WriteFile(pidfile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
				return 3
			}
		}
		time.Sleep(30 * time.Second)
		return 0
	case "wrapper":
		// Keep a child alive to test cancellation beyond the direct subprocess.
		body, err := json.Marshal(req)
		if err != nil {
			return 3
		}
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "STAVE_ADVISORY_HELPER=hang")
		child.Stdin, child.Stdout, child.Stderr = bytes.NewReader(body), os.Stdout, os.Stderr
		if err := child.Run(); err != nil {
			return 3
		}
		return 0
	}

	// Keep logs on stderr so stdout remains a single protocol response.
	fmt.Fprintln(os.Stderr, "helper: scoring", len(req.Files), "files")
	resp := Response{Schema: Schema, Backend: Backend{ID: "helper", Digest: "d", Detail: "echo"}, Classes: []string{"DC"}}
	yes, score := true, 0.75
	for _, f := range req.Files {
		resp.Files = append(resp.Files, FileRecord{Path: f.Path, SHA256: f.SHA256, Status: StatusChecked})
		p := Prediction{Path: f.Path, Class: "DC", SHA256: f.SHA256, Coverage: StatusChecked, Advice: &yes, Score: &score}
		p.Span.Start, p.Span.End = 1, 4
		resp.Predictions = append(resp.Predictions, p)
	}
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		return 3
	}
	return 0
}

// helper returns a Command provider that runs this test binary again as a backend in the given mode.
func helper(t *testing.T, mode string, timeout time.Duration) Command {
	t.Helper()
	t.Setenv("STAVE_ADVISORY_HELPER", mode)
	return Command{Argv: []string{os.Args[0]}, Timeout: timeout}
}

// wrapped returns a Command whose backend is a launcher around the process that does the work. It also returns the
// file that this process writes its pid to.
func wrapped(t *testing.T, timeout time.Duration) (Command, string) {
	t.Helper()
	pidfile := filepath.Join(t.TempDir(), "worker.pid")
	t.Setenv("STAVE_HELPER_PIDFILE", pidfile)
	return helper(t, "wrapper", timeout), pidfile
}

// Cleanup must kill the owned worker even if an assertion fails.
func workerPid(t *testing.T, pidfile string) int {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		body, err := os.ReadFile(pidfile)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
		if err != nil {
			t.Fatalf("pid file holds %q", body)
		}
		t.Cleanup(func() {
			if p, err := os.FindProcess(pid); err == nil && alive(pid) {
				_ = p.Kill()
			}
		})
		return pid
	}
	t.Fatal("the wrapped worker never reported its pid")
	return 0
}

// alive probes the process through the portable os API. On Windows, finding a process opens it, and this fails once
// the process is gone. On Unix, signal 0 is the existence check.
func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		p.Release()
		return true
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// Descendant cancellation requires process-group support.
func needsProcessGroups(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no process group on Windows: cancellation ends the direct child only")
	}
}

// gone reports whether the process ends within the deadline.
func gone(pid int, within time.Duration) bool {
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if !alive(pid) {
			return true
		}
	}
	return !alive(pid)
}

// Cancellation must reach the worker behind a launcher.
func TestATimeoutEndsTheWorkerBehindTheLauncher(t *testing.T) {
	needsProcessGroups(t)
	cmd, pidfile := wrapped(t, time.Second)

	_, err := cmd.Predict(context.Background(), request)

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want the timeout", err)
	}
	pid := workerPid(t, pidfile)
	if !gone(pid, 5*time.Second) {
		t.Errorf("worker %d is still alive after the exchange timed out", pid)
	}
}

func TestCancellationEndsTheWorkerBehindTheLauncher(t *testing.T) {
	needsProcessGroups(t)
	cmd, pidfile := wrapped(t, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := cmd.Predict(ctx, request)
		done <- err
	}()

	pid := workerPid(t, pidfile)
	cancel()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "cancel") {
			t.Errorf("err = %v, want the cancellation named", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Predict did not return after its context was cancelled")
	}
	if !gone(pid, 5*time.Second) {
		t.Errorf("worker %d is still alive after the exchange was cancelled", pid)
	}
}

// On every OS a timed-out exchange ends the backend it started, and returns without waiting for it.
func TestCommandTimeoutEndsTheBackend(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "backend.pid")
	t.Setenv("STAVE_HELPER_PIDFILE", pidfile)
	started := time.Now()

	_, err := helper(t, "hang", 500*time.Millisecond).Predict(context.Background(), request)

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want the timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("took %s: the exchange must not wait for a hung backend", elapsed)
	}
	pid := workerPid(t, pidfile)
	if !gone(pid, 5*time.Second) {
		t.Errorf("backend %d is still alive after the exchange timed out", pid)
	}
}

func TestCommandExchangesOneRequestForOneResponse(t *testing.T) {
	resp, err := helper(t, "echo", time.Minute).Predict(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Backend.ID != "helper" || len(resp.Files) != len(request.Files) || len(resp.Predictions) != len(request.Files) {
		t.Errorf("response = %+v", resp)
	}
	if _, err := Resolve(request, resp); err != nil {
		t.Errorf("the helper's response should resolve: %v", err)
	}
}

func TestCommandFailuresAreErrorsThatCarryTheirCause(t *testing.T) {
	for _, tc := range []struct {
		mode, want string
		timeout    time.Duration
	}{
		{"exit1", "model artifact is missing", time.Minute},
		{"garbage", "not one JSON object", time.Minute},
		{"hang", "timed out", 300 * time.Millisecond},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			started := time.Now()
			_, err := helper(t, tc.mode, tc.timeout).Predict(context.Background(), request)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one mentioning %q", err, tc.want)
			}
			if elapsed := time.Since(started); elapsed > 10*time.Second {
				t.Errorf("took %s: a hung backend must be killed at the timeout", elapsed)
			}
		})
	}

	if _, err := (Command{}).Predict(context.Background(), request); err == nil {
		t.Error("an empty argv must be an error")
	}
	if _, err := (Command{Argv: []string{filepath.Join(t.TempDir(), "absent")}}).Predict(context.Background(), request); err == nil {
		t.Error("a backend that cannot start must be an error")
	}
}

func TestRecordedAnswersOnlyForTheRequestedIdentities(t *testing.T) {
	// The recording knows shaA without a path, a stale revision of /pkg/b.go, and a source that nobody asked about.
	// Only the first applies.
	recording := `{"schema":"stave-advisory-v1","backend":{"id":"recorded-fixture","digest":"d","detail":"authored"},"classes":["DC"],
	  "files":[
	    {"sha256":"` + shaA + `","status":"checked","reason":null},
	    {"path":"/pkg/b.go","sha256":"` + shaC + `","status":"checked","reason":null},
	    {"sha256":"` + shaC + `","status":"checked","reason":null}],
	  "predictions":[
	    {"class":"DC","sha256":"` + shaA + `","span":{"start":1,"end":4},"coverage":"checked","advice":true,"score":0.9,"reason":null},
	    {"class":"DC","sha256":"` + shaC + `","span":{"start":2,"end":5},"coverage":"checked","advice":true,"score":0.9,"reason":null}]}`
	path := filepath.Join(t.TempDir(), "recorded.json")
	if err := os.WriteFile(path, []byte(recording), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := Recorded{Path: path}.Predict(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := Resolve(request, resp)
	if err != nil {
		t.Fatalf("a recording filtered to the request must resolve: %v", err)
	}
	if resp.Backend.ID != "recorded-fixture" || len(bound["/pkg/a.go"].Predictions) != 1 || len(bound["/pkg/copy.go"].Predictions) != 1 {
		t.Errorf("backend %+v, bound %+v", resp.Backend, bound)
	}
	if b := bound["/pkg/b.go"]; b.Record != nil {
		t.Errorf("/pkg/b.go bound %+v: a recording of another revision says nothing about this one", b)
	}
}

func TestRecordedFailsOnAnUnusableRecording(t *testing.T) {
	if _, err := (Recorded{Path: filepath.Join(t.TempDir(), "absent.json")}).Predict(context.Background(), request); err == nil {
		t.Error("a missing recording must be an error, not an empty answer")
	}
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (Recorded{Path: path}).Predict(context.Background(), request); err == nil {
		t.Error("a malformed recording must be an error")
	}
}
