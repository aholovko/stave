package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/aholovko/stave/internal/app"
	"github.com/aholovko/stave/internal/models"
)

func runModel(args []string, stdout, stderr io.Writer) int {
	arity := map[string]int{"list": 0, "add": 1, "use": 1, "remove": 1}
	if len(args) == 0 || arity[args[0]] != len(args)-1 || (args[0] != "list" && len(args) == 1) {
		fmt.Fprintf(stderr, "stave model: want list, add ID|URL|ZIP|DIR, use ID or remove ID\n%s", usage)
		return exitUsage
	}
	data, ok := dataDir("model", stderr)
	if !ok {
		return exitFailure
	}
	var err error
	switch args[0] {
	case "list":
		err = listModels(data, stdout)
	case "add":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		_, err = models.Add(ctx, data, args[1], checkBundle(ctx, data), stdout)
	case "use":
		if err = models.Use(data, args[1]); err == nil {
			fmt.Fprintf(stdout, "active model: %s (from the next check or save)\n", args[1])
		}
	case "remove":
		if err = models.Remove(data, args[1]); err == nil {
			fmt.Fprintf(stdout, "removed %s\n", args[1])
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "stave model %s: %v\n", args[0], err)
		return exitFailure
	}
	return exitOK
}

func listModels(data string, stdout io.Writer) error {
	installed, err := models.Installed(data)
	if err != nil {
		return err
	}
	c, err := app.LoadConfig(data)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tVARIANT\tVERSION\tDIGEST\tACTIVE")
	for _, m := range installed {
		active := ""
		if m.ID == c.Model {
			active = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.ID, m.Variant, m.Version, models.DigestPrefix(m.Model.SHA256), active)
	}
	return tw.Flush()
}
