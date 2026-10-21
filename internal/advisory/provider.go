package advisory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout allows a cold model load and matches the config.json default.
const DefaultTimeout = 180 * time.Second

const maxResponseBytes = 64 << 20

// Command exchanges one request and response over subprocess stdio. Success requires exit status zero.
// Argv is executed without a shell.
type Command struct {
	Argv    []string
	Timeout time.Duration
}

// Name identifies the provider kind in a report.
func (Command) Name() string { return "command" }

// Predict preserves failures as errors so a missing answer cannot be mistaken for "no advice".
func (c Command) Predict(ctx context.Context, req Request) (Response, error) {
	if len(c.Argv) == 0 {
		return Response{}, errors.New("advisory command has an empty argv")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	stdout, stderr := &capped{limit: maxResponseBytes}, &capped{limit: 64 << 10}
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(body), stdout, stderr
	cmd.WaitDelay = 2 * time.Second // an orphaned child of the backend must not hold the exchange open
	isolate(cmd)                    // On Unix, cancellation also ends selector children.

	err = cmd.Run()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return Response{}, fmt.Errorf("advisory command %s timed out after %s", c.Argv[0], timeout)
	case errors.Is(ctx.Err(), context.Canceled):
		return Response{}, fmt.Errorf("advisory command %s cancelled before it answered", c.Argv[0])
	case err != nil:
		return Response{}, fmt.Errorf("advisory command %s: %w%s", c.Argv[0], err, tail(stderr.buf.String()))
	case stdout.overflow:
		return Response{}, fmt.Errorf("advisory command %s wrote more than %d bytes", c.Argv[0], maxResponseBytes)
	}
	resp, err := Decode(&stdout.buf)
	if err != nil {
		return Response{}, fmt.Errorf("advisory command %s: %w%s", c.Argv[0], err, tail(stderr.buf.String()))
	}
	return resp, nil
}

// capped bounds retained output while draining the pipe so a verbose backend cannot block on it.
type capped struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room < len(p) {
		c.overflow = true
		c.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return c.buf.Write(p)
}

func tail(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	if len(stderr) > 2000 {
		stderr = "…" + stderr[len(stderr)-2000:]
	}
	return ": " + stderr
}

// Recorded exercises the pipeline with a saved response instead of a live model.
type Recorded struct {
	Path string
}

// Name identifies the provider kind in a report.
func (Recorded) Name() string { return "recorded" }

// Predict filters the recording to requested identities so stale revisions cannot contribute advice.
func (r Recorded) Predict(_ context.Context, req Request) (Response, error) {
	fh, err := os.Open(r.Path)
	if err != nil {
		return Response{}, fmt.Errorf("advisory recording: %w", err)
	}
	defer fh.Close()
	recording, err := Decode(fh)
	if err != nil {
		return Response{}, fmt.Errorf("advisory recording %s: %w", r.Path, err)
	}

	out := recording
	out.Files, out.Predictions = nil, nil
	for _, rec := range recording.Files {
		if _, err := applicable(req, rec.Path, rec.SHA256); err == nil {
			out.Files = append(out.Files, rec)
		}
	}
	for _, p := range recording.Predictions {
		if _, err := applicable(req, p.Path, p.SHA256); err == nil {
			out.Predictions = append(out.Predictions, p)
		}
	}
	return out, nil
}
