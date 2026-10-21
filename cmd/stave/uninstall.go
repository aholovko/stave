package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/aholovko/stave/internal/app"
)

// Allow only STAVE entries and harmless file-browser metadata.
func ownEntry(name string) bool {
	switch name {
	case app.ConfigFile, "runtime", "models", "downloads":
		return true
	}
	if slices.Contains(app.BrowserFiles, name) {
		return true
	}
	return strings.HasPrefix(name, ".config-") // an interrupted config.json write
}

// Refuse unknown entries so a mistaken STAVE_HOME cannot delete unrelated user files.
func runUninstall(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flagSet("uninstall", stderr)
	yes := flags.Bool("yes", false, "remove without asking")
	if code := parse(flags, args); code >= 0 {
		return code
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "stave uninstall: takes no arguments\n%s", usage)
		return exitUsage
	}
	data, ok := dataDir("uninstall", stderr)
	if !ok {
		return exitFailure
	}
	entries, err := os.ReadDir(data)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stdout, "nothing to remove: %s does not exist\n", data)
		return exitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, "stave uninstall: %v\n", err)
		return exitFailure
	}
	var foreign []string
	for _, e := range entries {
		if !ownEntry(e.Name()) {
			foreign = append(foreign, e.Name())
		}
	}
	if len(foreign) > 0 {
		fmt.Fprintf(stderr, "stave uninstall: %s holds files stave did not make (%s); nothing removed\n", data, strings.Join(foreign, ", "))
		return exitFailure
	}
	if !*yes {
		fmt.Fprintf(stdout, "Remove %s with its runtime and models? [y/N] ", data)
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(stdout, "nothing removed")
			return exitOK
		}
	}
	if err := os.RemoveAll(data); err != nil {
		fmt.Fprintf(stderr, "stave uninstall: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "removed %s; remove the stave binary and the VS Code extension to complete the uninstall\n", data)
	return exitOK
}
