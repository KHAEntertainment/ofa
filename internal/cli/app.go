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
	"ofa/internal/doctor"
	"ofa/internal/resolve"
	"ofa/internal/update"
)

// ExitCode is the process exit code returned by App.Run.
type ExitCode int

const (
	ExitOK             ExitCode = 0
	ExitUpdatesPending ExitCode = 1
	ExitError          ExitCode = 2
)

// version is the build version. It is a var, not a const, so the
// release workflow can stamp it with
// -ldflags="-X ofa/internal/cli.version=<version>".
//
// A const here would silently defeat the stamping: -X only rewrites
// string *variables*, and a const would leave the binary reporting a
// stale version no matter what tag was pushed. See
// cmd/ofa/version_test.go, which builds the binary with a stamp and
// asserts it actually shows up.
var version = "dev"

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

	// CatalogLoader returns the harnesses to scan against, plus one
	// catalog.Problem per user catalog file it had to skip. Default loads
	// the embedded TOML Catalog with the user catalog merged over it
	// (catalog.Merged over CatalogDir). Tests override to load a temp dir.
	CatalogLoader func() ([]catalog.Harness, []catalog.Problem, error)

	// CatalogDir is the user catalog directory merged over the embedded
	// one. Default = config.CatalogDir(); tests point it at a tempdir so a
	// developer's own ~/.config/ofa/catalog never changes their results.
	CatalogDir string

	// Pipeline functions. Defaults wire detect+resolve+update end-to-end;
	// tests can stub any layer to skip the shell.
	DetectFn  func(cat []catalog.Harness, run detect.Runner) ([]detect.Instance, []detect.Failure, error)
	ResolveFn func(insts []detect.Instance, cat []catalog.Harness, run resolve.Runner, get resolve.Getter, cfg resolve.Config) []resolve.Resolution

	// Runners default to real exec / HTTP.
	DetectRunner  detect.Runner
	ResolveRunner resolve.Runner
	ResolveGetter resolve.Getter
	UpdateExec    update.Executor

	// AgentFn drafts catalog entries for `ofa doctor`. Default runs Claude
	// Code headless and tool-less (doctor.ClaudeRunner); tests inject a
	// fake so no test ever spawns a real agent.
	AgentFn doctor.AgentRunner

	// DoctorDir is the root directory for doctor session artifacts.
	// Default = config.DoctorDir(); tests point it at a tempdir so a test
	// never writes into the developer's real ~/.config/ofa/doctor.
	DoctorDir string

	// State file path. Default = config.StatePath().
	StatePath string

	// Refresh forces resolve to re-query package registries instead of
	// serving fresh cached latest versions (issue #8). Set by --refresh
	// on check/apply/picker; default false.
	Refresh bool

	// Now is injected so tests can pin timestamps.
	Now func() time.Time

	// StderrIsTTY reports whether the picker may animate its loading
	// indicator (issue #12). The default reuses the stat-based isTerminal
	// check; tests override it to exercise both gates without needing a
	// real terminal.
	StderrIsTTY func(io.Writer) bool

	// ConfigLoader reads the user config (~/.config/ofa/config.toml).
	// Defaults to config.Load; tests override so they never read the
	// developer's real config.
	ConfigLoader func() (config.Config, error)

	// Pins is the pin set from the loaded config: harness ids the user
	// froze. Nil when nothing is pinned, and nil-safe to index.
	Pins map[string]bool
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
	a := &App{
		Args:          args,
		Stdin:         stdin,
		Stdout:        stdout,
		Stderr:        stderr,
		DetectFn:      detect.Scan,
		ResolveFn:     resolve.Resolve,
		DetectRunner:  detect.DefaultRunner,
		ResolveRunner: resolve.DefaultRunner,
		ResolveGetter: resolve.DefaultGetter,
		UpdateExec:    update.DefaultExecutor,
		AgentFn:       doctor.ClaudeRunner(),
		DoctorDir:     config.DoctorDir(),
		StatePath:     config.StatePath(),
		Now:           time.Now,
		StderrIsTTY:   stderrIsTTY,
		ConfigLoader:  config.Load,
		CatalogDir:    config.CatalogDir(),
	}
	// The default loader reads a.CatalogDir rather than config.CatalogDir()
	// directly, so a test can drive the real merge path against a fixture
	// directory without reaching into the developer's own catalog.
	a.CatalogLoader = func() ([]catalog.Harness, []catalog.Problem, error) {
		return catalog.Merged(a.CatalogDir)
	}
	return a
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
	case "doctor":
		return a.doctorCmd(a.Args[1:])
	case "--refresh", "-refresh":
		// Bare picker with the one flag it accepts. Pass the whole
		// argv (flag included) so pickerCmd's FlagSet actually parses
		// it — stripping it first made `ofa --refresh` a silent no-op.
		return a.pickerCmd(a.Args)
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
  ofa doctor [<bin>]  agent-assisted repair for undetected catalog entries
                      (opt-in, v1 --agent claude only; writes a proposal under
                      ~/.config/ofa/doctor/history/, never installs it)
  --refresh           re-query registries instead of cached latest versions
                      (accepted by: ofa, ofa check, ofa apply)
  ofa --version       print version
  ofa --help          this message`)
}

// loadConfig reads the user config and records its pins on the App. A
// malformed config is a hard error rather than a silent default: ignoring a
// broken pin would let OFA update something the user meant to freeze.
func (a *App) loadConfig() ExitCode {
	cfg, err := a.ConfigLoader()
	if err != nil {
		fmt.Fprintf(a.Stderr, "ofa: config: %v\n", err)
		return ExitError
	}
	a.Pins = cfg.PinSet()
	return ExitOK
}

// warnUnknownPins flags pin ids that do not exist in the catalog, so a typo
// does not silently pin nothing.
func (a *App) warnUnknownPins(cat []catalog.Harness) {
	if len(a.Pins) == 0 {
		return
	}
	known := make(map[string]bool, len(cat))
	for _, h := range cat {
		known[h.ID] = true
	}
	var unknown []string
	for id := range a.Pins {
		if !known[id] {
			unknown = append(unknown, id)
		}
	}
	sortStrings(unknown)
	for _, id := range unknown {
		fmt.Fprintf(a.Stderr, "ofa: warning: config pins %q, which is not in the catalog\n", id)
	}
}

// loadCatalog is a thin wrapper that reports skipped user catalog files and
// then prints any hard load error, returning a useful exit. Centralized so
// every command surfaces load failures the same way.
//
// A skipped user file is a warning, never a failure: the command runs against
// the embedded catalog plus every file that did load. Warnings go to Stderr so
// --json on stdout keeps its shape.
func (a *App) loadCatalog() ([]catalog.Harness, ExitCode) {
	cat, problems, err := a.CatalogLoader()
	for _, p := range problems {
		fmt.Fprintf(a.Stderr, "ofa: warning: skipping user catalog file %v\n", p)
	}
	if err != nil {
		fmt.Fprintf(a.Stderr, "ofa: catalog: %v\n", err)
		return nil, ExitError
	}
	return cat, ExitOK
}

// runDetect is the detect half of the pipeline, shared by check/apply/
// picker (via runPipeline) and by scan (which never resolves — issue #8).
// Detects installs, reports degraded detectors, and exits ExitError only
// when detection failed outright or when the failures left nothing at
// all to report, so a single broken package manager cannot zero a scan
// that other detectors could still answer for.
func (a *App) runDetect(cat []catalog.Harness) ([]detect.Instance, []detect.Failure, ExitCode) {
	insts, failures, err := a.DetectFn(cat, a.DetectRunner)
	if err != nil {
		fmt.Fprintf(a.Stderr, "ofa: scan: %v\n", err)
		return nil, nil, ExitError
	}
	if len(failures) > 0 {
		a.reportDetectionFailures(cat, insts, failures)
		if len(insts) == 0 {
			// Every result we would have shown came from a detector that
			// failed, so an empty report would be a lie about this machine.
			return nil, failures, ExitError
		}
	}
	return insts, failures, ExitOK
}

// runPipeline is the shared detect+resolve step used by check, apply, and
// the picker. Returns instances, resolutions, and the detector failures
// that degraded the run; any of the first two may be empty on error.
//
// Before detection it starts `brew outdated` in the background (issue
// #8): brew is the slowest single query in the pipeline, and overlapping
// it with the registry sweeps hides most of its cost. Resolution reads
// and refreshes the latest-version cache in the state file; a changed
// cache is written back only when a state file already exists — the
// state file is the scan record, and a latest-only file would corrupt
// drift.
func (a *App) runPipeline(cat []catalog.Harness) ([]detect.Instance, []resolve.Resolution, []detect.Failure, ExitCode) {
	var prefetch *resolve.BrewFuture
	if catHasBrewTier(cat) {
		prefetch = resolve.PrefetchBrew(a.ResolveRunner)
	}

	insts, failures, code := a.runDetect(cat)
	if code != ExitOK {
		// The prefetch completes into its own buffer and is collected.
		return nil, nil, failures, code
	}

	// Cache load is best-effort: a corrupt state file surfaces through
	// the commands' own loadState() exactly as it did before the cache
	// existed, while resolve itself degrades to uncached lookups.
	prev, _ := loadStateFile(a.StatePath)
	cache := newLatestCache(prev)
	res := a.ResolveFn(insts, cat, a.ResolveRunner, a.ResolveGetter, resolve.Config{
		Cache:    cache,
		Now:      a.Now,
		Refresh:  a.Refresh,
		Prefetch: prefetch,
	})
	a.warnStale(res)
	if prev != nil && cache.isDirty() {
		prev.Latest = cache.snapshot()
		if err := saveStateFile(a.StatePath, prev); err != nil {
			fmt.Fprintf(a.Stderr, "ofa: warning: could not cache latest versions: %v\n", err)
		}
	}
	return insts, res, failures, ExitOK
}

// catHasBrewTier reports whether the catalog has any brew-tier install,
// i.e. whether Resolve will need a `brew outdated` query at all — so a
// brew-less catalog never spawns one, prefetch or not.
func catHasBrewTier(cat []catalog.Harness) bool {
	for _, h := range cat {
		for _, in := range h.Installs {
			if in.Latest == "brew" {
				return true
			}
		}
	}
	return false
}

// warnStale reports resolutions served from cache because a refresh
// failed (issue #8). Stderr only, so check --json consumers stay clean.
func (a *App) warnStale(res []resolve.Resolution) {
	for _, r := range res {
		if !r.Stale {
			continue
		}
		age := a.Now().Sub(r.CachedAt)
		if age < 0 {
			age = 0
		}
		fmt.Fprintf(a.Stderr, "warning: %s latest for %s served from cache (%s old, fetched %s); refresh failed: %s\n",
			r.Tier, r.Package, age.Round(time.Minute), r.CachedAt.Format(time.RFC3339), r.RefreshErr)
	}
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
