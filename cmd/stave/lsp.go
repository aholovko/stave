package main

import (
	"fmt"
	"io"

	"github.com/aholovko/stave/internal/lsp"
)

// Keep operator messages on stderr because stdout carries LSP frames.
func runLSP(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var o options
	fs := flagSet("lsp", stderr)
	fs.Bool("stdio", true, "serve over standard input and output (the only transport)")
	o.buildFlags(fs)
	o.advisoryFlags(fs)
	if code := parse(fs, args); code >= 0 {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "stave lsp: takes no positional arguments\n%s", usage)
		return exitUsage
	}
	o.visited(fs)
	if _, err := o.config(); err != nil {
		fmt.Fprintf(stderr, "stave lsp: %v\n", err)
		return exitUsage
	}
	data, self, err := o.installation()
	if err != nil {
		fmt.Fprintf(stderr, "stave lsp: %v\n", err)
		return exitFailure
	}
	return lsp.Serve(o.configure(data, self), stdin, stdout, stderr)
}
