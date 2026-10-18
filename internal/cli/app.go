// Package cli wires the non-interactive and interactive command surface.
//
// The App struct holds the dispatch state (parsed args, I/O streams,
// injectable runners). Run() parses the top-level command, dispatches to
// the per-command implementation, and returns the process exit code.
//
// Exit codes:
//
//	0  success — no updates available, apply had no failures, scan succeeded
//	1  updates pending (check) OR apply had failures
//	2  usage / scan / runtime error
//
// The CLI never assumes a TTY; the apply command prompts only when the
// user did not pass --yes.
package cli

import (
	"fmt"
	"io"
	"os"
	"time"

	"ofa/internal/banner"
	"ofa/internal/catalog"
	"ofa/internal/config"
	"ofa/internal/detect"
	"ofa/internal/resolve"
	"ofa/internal/update"
)

// ExitCode is the process exit code returned by App.Run.
type ExitCode int

const (
	ExitOK              ExitCode = 0
	ExitUpdatesPending  ExitCode = 1
	ExitError           ExitCode = 2
)

// version is overridden at build time via -ldflags.
const version = "0.1.0-dev"

// App holds the dependencies for a single CLI invocation. New() fills in
// sensible defaults; tests override individual fields to inject stubs.
type App struct {
	// Args is os.Args[1:]. The first element is the subcommand.
	Args []string
	// I/O streams. Stdout receives human + JSON output. Stderr receives
	// logs, prompts, and errors. Stdin is read for the apply confirm.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// CatalogLoader returns the harnesses to scan against. Default loads
	// the embedded TOML Catalog. Tests override to load a temp dir.
	CatalogLoader func() ([]catalog.Harness, error)

	// Pipeline functions. Defaults wire detect+resolve+update end-to-end;
	// tests can stub any layer to skip the shell.
	DetectFn  func(cat []catalog.Harness, run detect.Runner) ([]detect.Instance, error)
	ResolveFn func(insts []detect.Instance, cat []catalog.Harness, run resolve.Runner, get resolve.Getter) []resolve.Resolution

	// Runners default to real exec / HTTP.
	DetectRunner  detect.Runner
	ResolveRunner resolve.Runner
	ResolveGetter resolve.Getter
	UpdateExec    update.Executor

	// State file path. Default = config.StatePath().
	StatePath string

	// Now is injected so tests can pin timestamps.
	Now func() time.Time
}

// New constructs an App with defaults wired.
//
// Stdin: nil is left as nil so callers (tests, mainly) can signal
// "no interactive input available" without us inferring "real terminal"
// from os.Stdin at construction time. The picker is the only command
// that needs to know; it inspects a.Stdin directly.
//
// Stdout / Stderr: nil falls back to os.Stdout / os.Stderr so production
// callers can pass nil and tests can pass buffers.
func New(args []string, stdin io.Reader, stdout, stderr io.Writer) *App {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	return &App{
		Args:          args,
		Stdin:         stdin,
		Stdout:        stdout,
		Stderr:        stderr,
		CatalogLoader: catalog.Embedded,
		DetectFn:      detect.Scan,
		ResolveFn:     resolve.Resolve,
		DetectRunner:  detect.DefaultRunner,
		ResolveRunner: resolve.DefaultRunner,
		ResolveGetter: resolve.DefaultGetter,
		UpdateExec:    update.DefaultExecutor,
		StatePath:     config.StatePath(),
		Now:           time.Now,
	}
}

// Run parses the top-level command and dispatches. Returns the process
// exit code; never panics on user input (always recovers to ExitError).
func (a *App) Run() ExitCode {
	if len(a.Args) == 0 {
		return a.pickerCmd(nil)
	}
	switch a.Args[0] {
	case "--version", "-V":
		fmt.Fprintf(a.Stdout, "ofa %s\n", version)
		return ExitOK
	case "-h", "--help", "help":
		// Print the banner above the help text so the brand surfaces
		// when a user runs `ofa --help` or hits an error. Other surfaces
		// (check, scan, apply's non-interactive path, --version) stay
		// plain.
		_ = banner.Render(a.Stderr)
		fmt.Fprintln(a.Stderr)
		a.usage()
		return ExitOK
	case "scan":
		return a.scanCmd(a.Args[1:])
	case "check", "status":
		return a.checkCmd(a.Args[1:])
	case "apply", "update":
		return a.applyCmd(a.Args[1:])
	default:
		fmt.Fprintf(a.Stderr, "ofa: unknown command %q\n", a.Args[0])
		a.usage()
		return ExitError
	}
}

func (a *App) usage() {
	fmt.Fprintln(a.Stderr, `ofa — One For All: detect and update coding harnesses

Usage:
  ofa                 interactive picker (default; auto-scans on first run)
  ofa scan            detect installed harnesses, write state, report drift
  ofa check (status)  non-interactive report of updates available
  ofa apply (update)  apply updates (--yes skips confirm, --only <id> narrows,
                      --dry-run/--plan prints without executing, --verbose
                      streams installer output; default shows progress lines)
  ofa check --json    machine-readable variant
  ofa --version       print version
  ofa --help          this message`)
}

// loadCatalog is a thin wrapper that prints the error and returns a
// useful exit. Centralized so every command surfaces load failures the
// same way.
func (a *App) loadCatalog() ([]catalog.Harness, ExitCode) {
	cat, err := a.CatalogLoader()
	if err != nil {
		fmt.Fprintf(a.Stderr, "ofa: catalog: %v\n", err)
		return nil, ExitError
	}
	return cat, ExitOK
}

// runPipeline is the shared scan+resolve step used by check, apply, and
// scan (well, scan does only the detect half). Returns instances and
// resolutions; either may be empty on error. Exits with ExitError on
// detect failure so callers don't repeat the boilerplate.
func (a *App) runPipeline(cat []catalog.Harness) ([]detect.Instance, []resolve.Resolution, ExitCode) {
	insts, err := a.DetectFn(cat, a.DetectRunner)
	if err != nil {
		fmt.Fprintf(a.Stderr, "ofa: scan: %v\n", err)
		return nil, nil, ExitError
	}
	res := a.ResolveFn(insts, cat, a.ResolveRunner, a.ResolveGetter)
	return insts, res, ExitOK
}

// loadState reads the state file via the state package. Returns nil +
// ExitOK when the file is missing (initial scan). Returns nil + ExitError
// on read/parse failure.
func (a *App) loadState() (*stateFile, ExitCode) {
	prev, err := loadStateFile(a.StatePath)
	if err != nil {
		fmt.Fprintf(a.Stderr, "ofa: state: %v\n", err)
		return nil, ExitError
	}
	return prev, ExitOK
}

// saveState writes the current scan via the state package. Returns
// ExitCode so callers can decide whether to continue after a save failure
// (we currently propagate it as ExitError).
func (a *App) saveState(f *stateFile) ExitCode {
	if err := saveStateFile(a.StatePath, f); err != nil {
		fmt.Fprintf(a.Stderr, "ofa: state: %v\n", err)
		return ExitError
	}
	return ExitOK
}