package doctor

import (
	"fmt"
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
// statically, and only then test-detect — which is the first point anything
// from the draft executes. It returns the parsed harness and the extracted
// TOML (what proposed-catalog.toml would contain) on success.
func ValidateProposal(out, bin string, run detect.Runner) (catalog.Harness, string, error) {
	tomlSrc := ExtractTOML(out)
	h, err := catalog.ParseEntry("proposed-catalog.toml", []byte(tomlSrc))
	if err != nil {
		return catalog.Harness{}, tomlSrc, err
	}
	if err := shapeGate(h, bin); err != nil {
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
// output. The gate ensures every name that would execute is the target
// binary itself and every argument is a plain flag: no path separators, no
// shell metacharacters, no control characters. update.cmd is never executed
// by doctor (the install flow only displays it for human review), so it is
// deliberately not gated here.
func shapeGate(h catalog.Harness, bin string) error {
	for i, in := range h.Installs {
		if in.Bin != bin {
			return fmt.Errorf("install[%d]: bin %q must be %q", i, in.Bin, bin)
		}
		if err := checkCmd(i, in.Version.Cmd, bin); err != nil {
			return err
		}
		if in.Method == "native" {
			for _, p := range in.Paths {
				if filepath.Base(p) != bin {
					return fmt.Errorf("install[%d]: native path %q must name the %q binary", i, p, bin)
				}
			}
		}
	}
	return nil
}

// checkCmd enforces the version command shape: cmd[0] must be the target bin
// itself (basename equality plus the plain-argument rule leaves no room for
// "./bin", a path, or any other executable), and every argument must be a
// plain flag.
func checkCmd(i int, cmd []string, bin string) error {
	if len(cmd) == 0 {
		return fmt.Errorf("install[%d]: missing version.cmd", i)
	}
	if cmd[0] != bin {
		return fmt.Errorf("install[%d]: version.cmd must start with %q, got %q", i, bin, cmd[0])
	}
	for _, arg := range cmd {
		if arg == "" {
			return fmt.Errorf("install[%d]: version.cmd contains an empty argument", i)
		}
		if err := plainArg(arg); err != nil {
			return fmt.Errorf("install[%d]: version.cmd argument %q: %v", i, arg, err)
		}
	}
	return nil
}

// plainArg rejects path separators, ':' and shell metacharacters, and control
// characters. Doctor execs without a shell, so none of these could be
// interpreted — but they are never legitimate in a version flag, and their
// presence means the draft is not what the schema asked for.
func plainArg(arg string) error {
	if strings.ContainsAny(arg, `/\:;&|<>$`+"`\"'") {
		return fmt.Errorf("path separator or shell metacharacter")
	}
	for _, r := range arg {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("control character")
		}
	}
	return nil
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
