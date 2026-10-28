package doctor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// AgentRunner drafts a catalog entry: it sends prompt to a headless agent
// and returns the agent's raw response text. The production implementation
// shells out to Claude Code; tests inject a fake so no test ever spawns a
// real agent. An AgentRunner never writes files — OFA writes everything
// itself — and its output is untrusted by contract.
type AgentRunner func(ctx context.Context, prompt string) (string, error)

// ClaudeCommand is the headless agent binary doctor shells out to. v1 is
// Claude-only: it is the one agent with a verified tool-less mode (Codex
// cannot disable tools, so it stays outside this seam until it can).
const ClaudeCommand = "claude"

// maxAgentOutput caps the bytes read from the agent before parsing, so a
// runaway response cannot flood memory, the prompt history, or the session
// files.
const maxAgentOutput = 64 * 1024

// claudeArgs is the verified tool-less invocation, kept as its own function
// so tests can assert the exact flag set:
//
//	-p                     print the response and exit
//	--safe-mode            drop CLAUDE.md, skills, plugins, hooks, MCP
//	--tools ""             disable every tool
//	--no-session-persistence leave nothing on disk
//	--permission-mode dontAsk never prompt for permission
func claudeArgs() []string {
	return []string{
		"-p",
		"--safe-mode",
		"--tools", "",
		"--no-session-persistence",
		"--permission-mode", "dontAsk",
	}
}

// ClaudeRunner returns the production AgentRunner.
//
// The prompt travels on stdin, not argv, so its contents never appear in a
// process listing. The process runs in a fresh empty temp directory — never
// the repo or $HOME — under a context with a timeout, and its stdout is
// capped at maxAgentOutput before anyone parses it. A non-zero exit or a
// timeout is an error, not a partial result.
func ClaudeRunner() AgentRunner {
	return func(ctx context.Context, prompt string) (string, error) {
		dir, err := os.MkdirTemp("", "ofa-doctor-")
		if err != nil {
			return "", fmt.Errorf("claude: temp dir: %w", err)
		}
		defer os.RemoveAll(dir)

		cmd := exec.CommandContext(ctx, ClaudeCommand, claudeArgs()...)
		cmd.Dir = dir
		cmd.Env = claudeEnv()
		cmd.Stdin = strings.NewReader(prompt)
		out := &cappedBuffer{limit: maxAgentOutput}
		var stderr bytes.Buffer
		cmd.Stdout = out
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("claude: %w", ctx.Err())
			}
			if msg := truncateRunes(stderr.String(), 400); msg != "" {
				return "", fmt.Errorf("claude: %w: %s", err, msg)
			}
			return "", fmt.Errorf("claude: %w", err)
		}
		if out.truncated {
			return out.String() + "\n…[output truncated by ofa]", nil
		}
		return out.String(), nil
	}
}

// claudeEnv is the minimal environment for the agent subprocess: enough to
// find the binary (PATH), the home directory (HOME), and the credentials a
// user who authenticates by environment variable actually needs. Nothing
// else crosses the boundary — no proxy settings, no other secrets, no OFA
// state.
func claudeEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TERM=dumb",
	}
	if v := os.Getenv("LANG"); v != "" {
		env = append(env, "LANG="+v)
	}
	for _, k := range []string{
		"CLAUDE_CONFIG_DIR",
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN",
	} {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// cappedBuffer is a bytes.Buffer that stops storing after limit bytes,
// flagging the cut. Write always reports success so the writer never sees an
// error — the overflow is simply discarded.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	room := w.limit - w.buf.Len()
	if room <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		w.buf.Write(p[:room])
		w.truncated = true
	} else {
		w.buf.Write(p)
	}
	return len(p), nil
}

func (w *cappedBuffer) String() string { return w.buf.String() }
