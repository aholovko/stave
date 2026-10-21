// Command stave shares saved-source analysis between the CLI and language server and manages its runtime and models.
// Exit codes are 0 for completion, 1 for failure and 2 for usage errors; findings are not failures.
// Stdout is reserved for results or LSP frames, and errors go to stderr.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/build"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aholovko/stave/internal/advisory"
	"github.com/aholovko/stave/internal/app"
	"github.com/aholovko/stave/internal/harness"
	"github.com/aholovko/stave/internal/pkgsnap"
)

const usage = `usage:
  stave check [-json] [build flags] [advisory flags] DIR|FILE.go...
  stave lsp [build flags] [advisory flags] [--stdio]
  stave setup [-no-model | -model ID|URL|ZIP|DIR]   set up the runtime and a model
  stave doctor                                      check the installation
  stave model list | add ID|URL|ZIP|DIR | use ID | remove ID
  stave uninstall [-yes]                            remove the data directory
  stave version
  stave structural --task P1|P2|P3|TF|DC|RP ...   (the inference backend's selector)

build flags:
  -tags a,b  -goos OS  -goarch ARCH
  -tests=true  analyse _test.go files as test targets, never as context
               (default: config.json "tests")

advisory flags (default: the active model of config.json):
  -advisory model|disabled|recorded|command
  -model ID                   another installed bundle, for this run
  -advisory-recorded FILE     a stave-advisory-v1 response object to replay
  -advisory-cmd JSON_ARGV     a backend command as a JSON array of strings
  -advisory-timeout 180s      (default: config.json "advisory_timeout")
`

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "check":
		return runCheck(args[1:], stdout, stderr)
	case "lsp":
		return runLSP(args[1:], stdin, stdout, stderr)
	case "setup":
		return runSetup(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "model":
		return runModel(args[1:], stdout, stderr)
	case "uninstall":
		return runUninstall(args[1:], stdin, stdout, stderr)
	case "version":
		return runVersion(args[1:], stdout, stderr)
	case "structural":
		return runStructural(args[1:], stdin, stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	fmt.Fprintf(stderr, "stave: unknown command %q\n%s", args[0], usage)
	return exitUsage
}

type options struct {
	tags, goos, goarch string
	tests              bool

	advisory, recorded, command, model string
	timeout                            time.Duration

	// set names the flags given on the command line. Those flags override config.json for the run.
	set map[string]bool
}

func (o *options) visited(fs *flag.FlagSet) {
	o.set = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { o.set[f.Name] = true })
}

// flagSet returns a flag set that reports to stderr and never exits.
func flagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("stave "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	return fs
}

func (o *options) buildFlags(fs *flag.FlagSet) {
	fs.StringVar(&o.tags, "tags", "", "comma-separated build tags")
	fs.StringVar(&o.goos, "goos", "", "target operating system (default: the host's)")
	fs.StringVar(&o.goarch, "goarch", "", "target architecture (default: the host's)")
	fs.BoolVar(&o.tests, "tests", app.DefaultConfig().Tests, "analyse _test.go files as test targets, never as context (default: config.json)")
}

func (o *options) advisoryFlags(fs *flag.FlagSet) {
	fs.StringVar(&o.advisory, "advisory", "model", "advisory provider: model, disabled, recorded, or command")
	fs.StringVar(&o.model, "model", "", "installed bundle to use instead of the active one")
	fs.StringVar(&o.recorded, "advisory-recorded", "", "recorded stave-advisory-v1 response to replay")
	fs.StringVar(&o.command, "advisory-cmd", "", "backend command as a JSON array of strings")
	fs.DurationVar(&o.timeout, "advisory-timeout", app.DefaultConfig().AdvisoryTimeout, "time allowed for one backend exchange (default: config.json)")
}

// parse reports the exit code to stop with, or -1 to continue.
func parse(fs *flag.FlagSet, args []string) int {
	switch err := fs.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		return exitOK
	case err != nil:
		return exitUsage
	}
	return -1
}

// config validates the shared flags before the installation is read. Every error it returns is a usage error. Model
// resolution, which reads the data directory, is deferred to resolve.
func (o *options) config() (harness.Config, error) {
	cfg := harness.Config{Build: o.build(), Key: o.advisory}
	if o.set["advisory-timeout"] && o.timeout <= 0 {
		return cfg, errors.New("-advisory-timeout must be a positive duration")
	}
	if o.model != "" && o.advisory != "model" {
		return cfg, errors.New("-model needs -advisory model")
	}
	switch o.advisory {
	case "model":
		if o.recorded != "" || o.command != "" {
			return cfg, errors.New("-advisory-recorded and -advisory-cmd need -advisory recorded or -advisory command")
		}
	case "disabled":
		if o.recorded != "" || o.command != "" {
			return cfg, errors.New("-advisory-recorded and -advisory-cmd need -advisory recorded or -advisory command")
		}
	case "recorded":
		if o.recorded == "" || o.command != "" {
			return cfg, errors.New("-advisory recorded needs -advisory-recorded FILE and no -advisory-cmd")
		}
		cfg.Provider = advisory.Recorded{Path: o.recorded}
		cfg.Key = "recorded:" + o.recorded
	case "command":
		var argv []string
		if o.recorded != "" {
			return cfg, errors.New("-advisory command takes no -advisory-recorded")
		}
		if err := json.Unmarshal([]byte(o.command), &argv); err != nil || len(argv) == 0 {
			return cfg, errors.New(`-advisory command needs -advisory-cmd as a JSON array of strings, such as '["python","-m","stave_infer","serve"]'`)
		}
		cfg.Provider = advisory.Command{Argv: argv, Timeout: o.timeout}
	default:
		return cfg, fmt.Errorf("-advisory %q is not model, disabled, recorded, or command", o.advisory)
	}
	return cfg, nil
}

// Follow Go's build defaults: cross-compilation disables cgo unless CGO_ENABLED=1 explicitly enables it.
func (o *options) build() pkgsnap.Build {
	b := pkgsnap.Build{GOOS: o.goos, GOARCH: o.goarch, Cgo: build.Default.CgoEnabled, TestTargets: o.tests}
	if b.GOOS == "" {
		b.GOOS = build.Default.GOOS
	}
	if b.GOARCH == "" {
		b.GOARCH = build.Default.GOARCH
	}
	if b.GOOS != build.Default.GOOS || b.GOARCH != build.Default.GOARCH {
		b.Cgo = os.Getenv("CGO_ENABLED") == "1"
	}
	for _, tag := range strings.Split(o.tags, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			b.Tags = append(b.Tags, tag)
		}
	}
	return b
}
