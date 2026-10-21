package main

// The inference runtime calls this selector through the same binary used by the editor.
// It consumes Go source bytes on stdin and emits ordered JSON results; candidate failures use ok=false.
// P2/P3/TF also write an enumeration ledger. P2 certification requires stdin to match --target on disk.
// TF gates --path as text without opening it and checks stdin against --sha256.
// DC/RP report withheld candidate counts on stderr. Exit 2 means usage error; exit 1 invalid source or output failure.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/aholovko/stave/internal/pkgsnap"
	"github.com/aholovko/stave/internal/structural"
)

func runStructural(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("stave-structural", flag.ContinueOnError)
	flags.SetOutput(stderr)
	task := flags.String("task", "", "candidate task: P1, P2, P3, TF, DC or RP")
	id := flags.String("id", "stdin", "source ID used in candidate IDs")
	ledgerPath := flags.String("ledger", "", "P2/P3/TF: file the enumeration ledger is written to")
	target := flags.String("target", "", "P2: the source's file, whose package directory is certified")
	path := flags.String("path", "", "TF: the source's path, gated as text and never opened")
	sha := flags.String("sha256", "", "TF: the expected SHA-256 of the stdin bytes")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: stave structural --task P1|DC|RP [--id ID] < SOURCE")
		fmt.Fprintln(stderr, "       stave structural --task P2|P3 --ledger PATH [--target FILE] [--id ID] < SOURCE")
		fmt.Fprintln(stderr, "       stave structural --task TF --ledger PATH --path P --sha256 HEX [--id ID] < SOURCE")
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	packet := *task == "DC" || *task == "RP"
	direct := *task == "P1" || packet
	tf := *task == "TF"
	ledgerTask := *task == "P2" || *task == "P3" || tf
	if (!direct && !ledgerTask) || flags.NArg() != 0 || ledgerTask != (*ledgerPath != "") ||
		(*target != "" && *task != "P2") || tf != (*path != "") || tf != (*sha != "") {
		flags.Usage()
		return 2
	}
	src, err := io.ReadAll(stdin)
	if err != nil {
		return failStructural(stderr, err)
	}
	if tf {
		streams, ledger, err := structural.BuildTFLedger(*id, *path, *sha, src)
		if err != nil {
			return failStructural(stderr, err)
		}
		return publishLedger(ledger, streams, *ledgerPath, stdout, stderr)
	}
	if ledgerTask {
		return runLedger(*task, *id, *ledgerPath, *target, src, stdout, stderr)
	}
	if packet {
		streams, withheld, err := structural.BuildDCRP(*task, *id, src)
		if err != nil {
			return failStructural(stderr, err)
		}
		if code := writeResults(streams, stdout, stderr); code != 0 {
			return code
		}
		fmt.Fprintf(stderr, "stave-structural: %s withheld %v\n", *task, withheld)
		return 0
	}
	streams, err := structural.Build(*task, *id, src)
	if err != nil {
		return failStructural(stderr, err)
	}
	return writeResults(streams, stdout, stderr)
}

func writeResults(streams []structural.Stream, stdout, stderr io.Writer) int {
	out := bufio.NewWriter(stdout)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for _, s := range streams {
		if err := enc.Encode(structural.Select(s.Header, s.Events)); err != nil {
			return failStructural(stderr, err)
		}
	}
	if err := out.Flush(); err != nil {
		return failStructural(stderr, err)
	}
	return 0
}

// Build and validate everything before writing so input or identity failures leave no output.
func runLedger(task, id, ledgerPath, target string, src []byte, stdout, stderr io.Writer) int {
	var cert *pkgsnap.Certificate
	if target != "" {
		c, err := pkgsnap.Certify(target)
		if err != nil {
			return failStructural(stderr, err)
		}
		cert = c
	}
	streams, ledger, err := structural.BuildLedger(task, id, src, cert)
	if err != nil {
		return failStructural(stderr, err)
	}
	return publishLedger(ledger, streams, ledgerPath, stdout, stderr)
}

// Output is usable only if every write succeeds.
func publishLedger(ledger any, streams []structural.Stream, ledgerPath string, stdout, stderr io.Writer) int {
	results := make([]structural.Result, len(streams))
	for i, s := range streams {
		results[i] = structural.Select(s.Header, s.Events)
	}
	var doc bytes.Buffer
	enc := json.NewEncoder(&doc)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ledger); err != nil {
		return failStructural(stderr, err)
	}
	if err := os.WriteFile(ledgerPath, doc.Bytes(), 0o644); err != nil {
		return failStructural(stderr, err)
	}
	out := bufio.NewWriter(stdout)
	enc = json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for _, r := range results {
		if err := enc.Encode(r); err != nil {
			return failStructural(stderr, err)
		}
	}
	if err := out.Flush(); err != nil {
		return failStructural(stderr, err)
	}
	return 0
}

// Keep the diagnostic prefix stable for backend consumers.
func failStructural(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "stave-structural:", err)
	return 1
}
