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
// run. Test-detect executes version.cmd, and for native installs it executes
// the paths entries — commands an agent drafted from untrusted --help
// output. The gate ensures every executed name is the authorized target
// binary itself and every version command is a read-only version query.
// update.cmd is never executed by doctor (the install flow only displays it
// for human review), so it is deliberately not gated here.
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

// versionFlags is the exhaustive allowlist of read-only version query
// argument vectors: a draft's version.cmd must be exactly [bin, flag] with
// flag from this set. Character filtering cannot establish that a command
// is a read-only version query — ["bin", "-c", "id"] and
// ["claude", "-p", "run id"] contain no metacharacters, and test-detect
// would execute them — so anything outside the allowlist is rejected before
// any Runner call.
var versionFlags = map[string]bool{
	"--version": true,
	"-version":  true,
	"-V":        true,
	"-v":        true,
	"version":   true,
}

// checkCmd enforces the version command shape: exactly [bin, flag], with
// the flag from the read-only allowlist. No arguments at all is rejected
// too — there is no way to justify running the bare binary as a "version
// query".
func checkCmd(i int, cmd []string, bin string) error {
	if len(cmd) != 2 {
		return fmt.Errorf("install[%d]: version.cmd must be exactly [%q, <flag>], got %q", i, bin, cmd)
	}
	if cmd[0] != bin {
		return fmt.Errorf("install[%d]: version.cmd must start with %q, got %q", i, bin, cmd[0])
	}
	if !versionFlags[cmd[1]] {
		return fmt.Errorf("install[%d]: version.cmd flag %q is not a read-only version flag (one of --version, -version, -V, -v, version)", i, cmd[1])
	}
	return nil
}

// checkNativePaths authorizes every native probe path against the target's
// executable identity. detectNative EXECUTES these paths, so basename
// equality is not enough — a different file or a symlink to another binary
// with the target's basename would be executed. After ~ expansion each path
// must be absolute, contain no ".." segment, and have the target's basename;
// it must then either not exist (it will never be executed — the
// correctness gate rejects the draft) or resolve to exactly the file PATH
// authorized as the target, by filepath.EvalSymlinks equality. A symlink
// chain to the target itself is the legitimate case that survives.
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
