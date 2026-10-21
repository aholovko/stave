package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/aholovko/stave/internal/app"
	"github.com/aholovko/stave/internal/models"
	"github.com/aholovko/stave/internal/runtime"
)

// Setup is repeatable: working runtimes and installed bundles are reused.
func runSetup(args []string, stdout, stderr io.Writer) int {
	fs := flagSet("setup", stderr)
	noModel := fs.Bool("no-model", false, "set up the runtime only")
	model := fs.String("model", "", "the bundle to install: a catalog id, URL, zip or directory (default: the catalog's default)")
	if code := parse(fs, args); code >= 0 {
		return code
	}
	if fs.NArg() != 0 || (*noModel && *model != "") {
		fmt.Fprintf(stderr, "stave setup: takes no arguments, and -model or -no-model, not both\n%s", usage)
		return exitUsage
	}
	data, ok := dataDir("setup", stderr)
	if !ok {
		return exitFailure
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runtime.Setup(ctx, data, stdout); err != nil {
		fmt.Fprintf(stderr, "stave setup: %v\n", err)
		return exitFailure
	}
	if !*noModel {
		source := *model
		if source == "" {
			source = models.EmbeddedCatalog().Default
		}
		if m, _, err := models.Bundle(data, source); err == nil {
			fmt.Fprintf(stdout, "model %s is installed\n", m.ID)
			// Restore a missing selection without replacing a valid one.
			if _, _, err := models.Active(data); err != nil {
				if err := models.Use(data, m.ID); err != nil {
					fmt.Fprintf(stderr, "stave setup: %v\n", err)
					return exitFailure
				}
			}
		} else if _, err := models.Add(ctx, data, source, checkBundle(ctx, data), stdout); err != nil {
			fmt.Fprintf(stderr, "stave setup: %v\n", err)
			return exitFailure
		}
	}
	fmt.Fprintf(stdout, "data directory: %s\n", data)
	if m, _, err := models.Active(data); err == nil {
		fmt.Fprintf(stdout, "active model: %s\n", m.ID)
	} else {
		fmt.Fprintln(stdout, "no model installed yet; add one with stave model add ID|URL|ZIP|DIR")
	}
	return exitOK
}

func dataDir(command string, stderr io.Writer) (string, bool) {
	dir, err := app.DataDir()
	if err != nil {
		fmt.Fprintf(stderr, "stave %s: %v\n", command, err)
		return "", false
	}
	return dir, true
}

func checkBundle(ctx context.Context, data string) func(string) error {
	return func(dir string) error { return runtime.CheckBundle(ctx, data, dir) }
}
