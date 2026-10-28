package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ofa/internal/catalog"
	"ofa/internal/detect"
)

// maxAttempts is the initial agent call plus the single retry the spec
// allows. The retry carries the validation error back to the agent; after
// that the draft is rejected and saved raw.
const maxAttempts = 2

// Attempt records one agent round trip: the prompt sent, the raw response,
// and the validation verdict — "" means accepted, otherwise the exact error
// the mechanical gate returned.
type Attempt struct {
	Prompt string
	Output string
	Err    string
}

// ValidateProposal runs one agent output through the full mechanical gate,
// in the order that matters: extract, parse and validate, gate the commands
// statically, and only then test-detect — the first point anything from the
// draft executes. targetReal is the symlink-resolved path of the authorized
// target binary (the LookPath winner); the native-path gate binds every
// probe path to it. It returns the parsed harness and the extracted TOML
// (what proposed-catalog.toml would contain) on success.
func ValidateProposal(out, bin, targetReal string, run detect.Runner) (catalog.Harness, string, error) {
	tomlSrc := ExtractTOML(out)
	h, err := catalog.ParseEntry("proposed-catalog.toml", []byte(tomlSrc))
	if err != nil {
		return catalog.Harness{}, tomlSrc, err
	}
	if err := shapeGate(h, bin, targetReal); err != nil {
		return catalog.Harness{}, tomlSrc, err
	}
	if err := testDetect(h, bin, run); err != nil {
		return catalog.Harness{}, tomlSrc, err
	}
	return h, tomlSrc, nil
}

// ExtractTOML pulls the draft out of the agent's response: the first
// ```toml fence when present, otherwise the whole output trimmed. The fence
// wins because agents wrap TOML in prose despite instructions; the fallback
// keeps a bare-TOML response usable. An unterminated fence yields everything
// after its opening line, which then fails parsing like any other malformed
// draft.
func ExtractTOML(out string) string {
	const fence = "```toml"
	i := strings.Index(out, fence)
	if i < 0 {
		return strings.TrimSpace(out)
	}
	rest := out[i+len(fence):]
	if j := strings.Index(rest, "```"); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

// shapeGate statically inspects a parsed proposal BEFORE any of its commands
// run. It is the first line of defense — clear, early feedback to the model
// and to the one retry — while the bounded runner is the enforcement at
// execution time: a draft that slips past this gate still cannot execute
// anything but the pinned target (see runner.go). Test-detect executes
// version.cmd, and for native installs it probes the paths entries —
// commands an agent drafted from untrusted --help output. update.cmd is
// never executed by doctor (the install flow only displays it for human
// review), so it is deliberately not gated here.
func shapeGate(h catalog.Harness, bin, targetReal string) error {
	for i, in := range h.Installs {
		if in.Bin != bin {
			return fmt.Errorf("install[%d]: bin %q must be %q", i, in.Bin, bin)
		}
		if err := checkCmd(i, in.Version.Cmd, bin); err != nil {
			return err
		}
		if in.Method == "native" {
			if err := checkNativePaths(i, in.Paths, bin, targetReal); err != nil {
				return err
			}
		}
	}
	return nil
}

// versionFlags is the read-only version query argument allowlist. It has
// exactly one entry — "--version" — the vector the facts phase has already
// proven for this binary and the vector every shipped catalog entry uses.
// Character filtering cannot establish that a command is read-only, and
// wider allowlists ("-v", "version", ...) authorize active behavior on
// harnesses the catalog has never seen, so anything outside this set is
// rejected before any Runner call.
var versionFlags = map[string]bool{
	"--version": true,
}

// checkCmd enforces the version command: exactly [bin, "--version"]. The
// facts phase has already run `<bin> --version` successfully before
// drafting, and every shipped catalog entry uses exactly this form — so
// one form, no judgment calls. Anything else is rejected before any Runner
// call: bare "version" is a positional prompt on some harnesses (pi's
// `version` in non-TTY starts a tool-enabled model session), "-v" is
// verbose or ambiguous on others, and "-c"/"-p"/extra arguments are
// active commands. The error text tells the model exactly what to use, and
// it is what the one retry feeds back.
func checkCmd(i int, cmd []string, bin string) error {
	if len(cmd) != 2 || cmd[0] != bin || cmd[1] != "--version" {
		return fmt.Errorf("install[%d]: version.cmd must be exactly [%q, \"--version\"], got %q", i, bin, cmd)
	}
	return nil
}

// checkNativePaths authorizes every native probe path against the target's
// executable identity — statically, for feedback; the bounded runner pins
// what actually executes (R2-B2). detectNative probes these paths, so
// basename equality is not enough. After ~ expansion each path must be
// absolute, contain no ".." segment, and have the target's basename; it
// must then either not exist (the correctness gate rejects the draft, and
// even if the file appears later the runner still executes only the pinned
// target) or resolve to exactly the file PATH authorized as the target, by
// filepath.EvalSymlinks equality. A symlink chain to the target itself is
// the legitimate case that survives.
func checkNativePaths(i int, paths []string, bin, targetReal string) error {
	for _, p := range paths {
		expanded := expandTilde(p)
		if !filepath.IsAbs(expanded) {
			return fmt.Errorf("install[%d]: native path %q must be absolute", i, p)
		}
		for _, seg := range strings.Split(expanded, string(filepath.Separator)) {
			if seg == ".." {
				return fmt.Errorf("install[%d]: native path %q must not contain \"..\"", i, p)
			}
		}
		if filepath.Base(expanded) != bin {
			return fmt.Errorf("install[%d]: native path %q must name the %q binary", i, p, bin)
		}
		st, err := os.Stat(expanded)
		if errors.Is(err, os.ErrNotExist) {
			continue // never executed; the correctness gate handles it
		}
		if err != nil {
			return fmt.Errorf("install[%d]: native path %q: %w", i, p, err)
		}
		if st.IsDir() {
			return fmt.Errorf("install[%d]: native path %q is a directory", i, p)
		}
		real, err := filepath.EvalSymlinks(expanded)
		if err != nil {
			return fmt.Errorf("install[%d]: native path %q: %w", i, p, err)
		}
		if real != targetReal {
			return fmt.Errorf("install[%d]: native path %q resolves to %q, not the authorized %q binary", i, p, real, bin)
		}
	}
	return nil
}

// expandTilde expands a leading ~ to the home directory, the same way
// detect expands native probe paths.
func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// testDetect is the main correctness gate: the proposal must actually detect
// the target through the real detect path. Scan runs over exactly the
// proposed harness — an id that collides with an embedded entry is fine
// here, because detection is a property of the entry, not of its neighbors —
// and requires an instance whose Bin is the target. run is the same runner
// the production scan uses; only shape-gated commands and detect's own fixed
// registry queries run through it.
func testDetect(h catalog.Harness, bin string, run detect.Runner) error {
	insts, _, err := detect.Scan([]catalog.Harness{h}, run)
	if err != nil {
		return fmt.Errorf("test-detect failed: %w", err)
	}
	for _, in := range insts {
		if in.Bin == bin {
			return nil
		}
	}
	return fmt.Errorf("proposed entry does not detect %q: the scan found no instance for this binary", bin)
}
