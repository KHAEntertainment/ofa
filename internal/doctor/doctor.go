// Package doctor implements `ofa doctor`: agent-assisted repair for catalog
// entries whose binary is installed but not detected. Doctor gathers facts
// about one binary, has a headless, tool-less agent draft a Catalog TOML
// entry from those facts, then mechanically validates the draft before anyone
// acts on it: it must parse and validate as a catalog entry, every command it
// declares must name the target binary itself, and — the main correctness
// gate — it must detect the target through the real detect path. Nothing here
// installs the proposal into the user catalog; that is the follow-on flow.
//
// The agent sits on the far side of a trust boundary, and everything it
// returns is treated as untrusted input. The defenses are layered and none of
// them is the prompt: the agent runs with no tools and no session persistence
// in a fresh empty directory, the drafted commands are gated statically
// before anything is executed, the proposal must survive the real detect
// path, and the human reviews the file before it is ever installed.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"ofa/internal/catalog"
	"ofa/internal/detect"
)

// DefaultTimeout bounds one agent call. A headless draft that takes longer
// than this is stuck, not thoughtful.
const DefaultTimeout = 120 * time.Second

// Options carries everything one doctor session needs, injected so tests
// never spawn a real agent or touch the user's real config directory.
type Options struct {
	// Agent drafts the TOML. Required. Production wires ClaudeRunner.
	Agent AgentRunner
	// Runner runs commands for the facts and the test-detect pass, the
	// same Runner seam detect uses. Required.
	Runner detect.Runner
	// LookPath resolves a bin name on PATH; defaults to exec.LookPath.
	LookPath func(string) (string, error)
	// Timeout bounds each agent call; defaults to DefaultTimeout.
	Timeout time.Duration
	// DoctorDir is the doctor root (config.DoctorDir()); the session
	// lands in DoctorDir/history/<ts>/. Required.
	DoctorDir string
	// Now stamps the session directory; defaults to time.Now.
	Now func() time.Time
}

// Result is what one session produced. Dir is always set once the session
// got as far as creating its directory; the paths are set only on the
// outcome they name. A non-nil error alongside a Result means the failure
// left evidence in Dir (context.md, and rejected.txt for a rejected draft).
type Result struct {
	// Dir is the session directory history/<ts>/.
	Dir string
	// Harness is the accepted, validated proposal; nil unless accepted.
	Harness *catalog.Harness
	// ProposalPath is proposed-catalog.toml; empty unless accepted.
	ProposalPath string
	// RejectedPath is rejected.txt; empty unless the draft was rejected.
	RejectedPath string
	// Attempts records every agent round trip, in order.
	Attempts []Attempt
}

// Run drives one doctor session for bin: facts, prompt, session directory,
// then up to maxAttempts agent calls, each validated mechanically. It never
// writes anywhere but the session directory.
//
// Errors distinguish three shapes: before any file exists (bin not on PATH,
// misconfigured options), agent failures (timeout, non-zero exit — the
// session directory and context.md still exist, per the failure-mode table),
// and a rejected proposal (rejected.txt written, one retry already spent).
func Run(bin string, opts Options) (*Result, error) {
	if opts.Agent == nil {
		return nil, errors.New("doctor: no agent runner configured")
	}
	if opts.Runner == nil {
		return nil, errors.New("doctor: no command runner configured")
	}
	if opts.DoctorDir == "" {
		return nil, errors.New("doctor: no doctor directory configured")
	}
	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	// Before any file exists: a bin that is not on PATH has nothing to
	// gather facts about and nothing to propose.
	path, err := lookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("doctor: %q is not on PATH", bin)
	}

	facts := CollectFacts(bin, path, opts.Runner)
	prompt := BuildPrompt(bin, facts)

	root := filepath.Join(opts.DoctorDir, "history")
	dir, err := NewSessionDir(root, now())
	if err != nil {
		return nil, fmt.Errorf("doctor: %w", err)
	}
	res := &Result{Dir: dir}
	save := func() error {
		if err := writeSession(dir, bin, facts, res.Attempts); err != nil {
			return fmt.Errorf("doctor: %w", err)
		}
		return nil
	}
	// record stamps the session's verdict into session.json so --list can
	// show it and --install can name the target back. Verdicts: proposed
	// when a validated proposal waits for a human, rejected when the
	// session ends without one (failed agent run or failed validation).
	// "installed" is written later by the install flow, not here.
	record := func(status, harnessID string) error {
		if err := SaveSessionRecord(dir, SessionRecord{
			Target:    bin,
			HarnessID: harnessID,
			Time:      now().UTC(),
			Status:    status,
		}); err != nil {
			return fmt.Errorf("doctor: %w", err)
		}
		return nil
	}
	// context.md exists before the agent is ever spawned, so a timeout or
	// crash still leaves the audit trail the failure-mode table requires.
	if err := save(); err != nil {
		return res, err
	}

	var lastErr error
	for n := 1; n <= maxAttempts; n++ {
		cur := prompt
		if n > 1 {
			cur = RetryPrompt(prompt, lastErr)
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		out, aerr := opts.Agent(ctx, cur)
		cancel()
		att := Attempt{Prompt: cur, Output: out}
		if aerr != nil {
			att.Err = aerr.Error()
			res.Attempts = append(res.Attempts, att)
			if err := save(); err != nil {
				return res, err
			}
			if err := record(StatusRejected, ""); err != nil {
				return res, err
			}
			return res, fmt.Errorf("doctor: agent run failed: %w", aerr)
		}
		h, tomlSrc, verr := ValidateProposal(out, bin, facts.RealPath, opts.Runner)
		if verr == nil {
			res.Attempts = append(res.Attempts, att)
			res.Harness = &h
			proposal := filepath.Join(dir, "proposed-catalog.toml")
			if err := os.WriteFile(proposal, []byte(tomlSrc), 0o600); err != nil {
				return res, fmt.Errorf("doctor: %w", err)
			}
			res.ProposalPath = proposal
			if err := save(); err != nil {
				return res, err
			}
			if err := record(StatusProposed, h.ID); err != nil {
				return res, err
			}
			return res, nil
		}
		att.Err = verr.Error()
		res.Attempts = append(res.Attempts, att)
		lastErr = verr
		if err := save(); err != nil {
			return res, err
		}
	}

	rejected := filepath.Join(dir, "rejected.txt")
	if err := os.WriteFile(rejected, []byte(rejectedContent(lastErr, res.Attempts)), 0o600); err != nil {
		return res, fmt.Errorf("doctor: %w", err)
	}
	res.RejectedPath = rejected
	if err := record(StatusRejected, ""); err != nil {
		return res, err
	}
	return res, fmt.Errorf("doctor: proposal rejected: %w", lastErr)
}
