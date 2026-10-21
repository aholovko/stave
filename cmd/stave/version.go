package main

import (
	"fmt"
	"io"

	"github.com/aholovko/stave/internal/app"
	"github.com/aholovko/stave/internal/models"
	"github.com/aholovko/stave/internal/runtime"
)

// Report installation state without starting the runtime.
func runVersion(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintf(stderr, "stave version: takes no arguments\n%s", usage)
		return exitUsage
	}
	fmt.Fprintf(stdout, "stave %s\n", app.Version)
	data, ok := dataDir("version", stderr)
	if !ok {
		return exitFailure
	}
	fmt.Fprintf(stdout, "runtime: %s\n", runtime.Status(data))
	if m, _, err := models.Active(data); err != nil {
		fmt.Fprintf(stdout, "model: none (%v)\n", err)
	} else {
		fmt.Fprintf(stdout, "model: %s (variant %s, version %s, model sha256 %s)\n", m.ID, m.Variant, m.Version, models.DigestPrefix(m.Model.SHA256))
	}
	return exitOK
}
