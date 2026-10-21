package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/aholovko/stave/internal/models"
	"github.com/aholovko/stave/internal/runtime"
)

// selfTestSource has one P1 candidate, the else after the direct return.
const selfTestSource = "package p\n\nfunc f(a int) int {\n\tif a > 0 {\n\t\treturn 1\n\t} else {\n\t\treturn 2\n\t}\n}\n"

func runDoctor(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintf(stderr, "stave doctor: takes no arguments\n%s", usage)
		return exitUsage
	}
	data, ok := dataDir("doctor", stderr)
	if !ok {
		return exitFailure
	}
	ctx := context.Background()
	failed := false
	report := func(check, ok string, err error) {
		if err != nil {
			failed = true
			fmt.Fprintf(stdout, "FAIL %s: %v\n", check, err)
			return
		}
		fmt.Fprintf(stdout, "OK %s %s\n", check, ok)
	}

	// Only stave setup runs uv. A runtime that starts does not need it.
	version, err := runtime.Version(ctx, data)
	if err == nil {
		report("uv", "not needed (runtime set up)", nil)
	} else {
		uv, uvErr := runtime.FindUV()
		report("uv", uv, uvErr)
	}

	if err == nil && !strings.Contains(version, "protocol stave-advisory-v1") {
		err = fmt.Errorf("the runtime reports %q, not the stave-advisory-v1 protocol; run stave setup", version)
	}
	report("runtime", runtime.Status(data), err)
	runtimeErr := err

	// Hashes verify file identity; loading the bundle checks compatibility beyond the lightweight version probe.
	m, dir, err := models.Active(data)
	if err == nil {
		err = models.VerifyBundle(dir)
	}
	if err == nil && runtimeErr == nil {
		if err = runtime.CheckBundle(ctx, data, dir); err != nil {
			err = fmt.Errorf("%v; rerun stave setup", err)
		}
	}
	if err != nil {
		report("model", "", err)
	} else {
		status := "files verified; load not checked"
		if runtimeErr == nil {
			status = "loads"
		}
		report("model", fmt.Sprintf("%s (variant %s, version %s, model sha256 %s; %s)", m.ID, m.Variant, m.Version, models.DigestPrefix(m.Model.SHA256), status), nil)
	}

	exe, err := os.Executable()
	if err == nil {
		err = selfTest(ctx, exe)
	}
	report("selector", exe+" structural", err)

	if failed {
		return exitFailure
	}
	return exitOK
}

// Exercise the same selector invocation used by the runtime.
func selfTest(ctx context.Context, exe string) error {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, exe, "structural", "--task", "P1", "--id", "doctor")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(selfTestSource), &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s; reinstall stave", err, strings.TrimSpace(stderr.String()))
	}
	if n := strings.Count(stdout.String(), "\n"); n != 1 {
		return fmt.Errorf("the selector found %d candidates in its self-test source, not 1; reinstall stave", n)
	}
	return nil
}
