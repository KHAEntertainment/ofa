package doctor

import (
	"context"
	"fmt"
	"os"
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
//	-p                        print the response and exit
//	--safe-mode               drop CLAUDE.md, skills, plugins, hooks, MCP
//	--tools ""                disable every tool
//	--no-session-persistence  leave nothing on disk
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
// process listing. The subprocess runs through execBounded: a fresh empty
// temp working directory (never the repo or $HOME), a minimal environment,
// the caller's context (Run sets the agent timeout), a streaming cap on
// both stdout and stderr, and a process-group kill — so a grandchild
// holding stdout cannot stretch the timeout into an unbounded hang, and a
// timed-out or failed claude is actually gone. On a non-zero exit the error
// carries a bounded snippet of stdout and stderr, because claude prints its
// failures on stdout.
func ClaudeRunner() AgentRunner {
	return func(ctx context.Context, prompt string) (string, error) {
		out, errOut, truncated, err := execBounded(ctx, ClaudeCommand, claudeArgs(), execSpec{
			timeout:   DefaultTimeout,
			env:       claudeEnv(),
			stdin:     strings.NewReader(prompt),
			maxStdout: maxAgentOutput,
			maxStderr: commandStderrCap,
		})
		if err != nil {
			return "", fmt.Errorf("claude: %w%s", err, streamSnippet(out, errOut))
		}
		if truncated {
			return out + "\n…[output truncated by ofa]", nil
		}
		return out, nil
	}
}

// claudeEnv is the minimal environment for the agent subprocess: enough to
// find the binary (PATH), the home directory (HOME), the credentials a user
// who authenticates by environment variable needs, and the identity
// variables macOS's keychain reads need (USER, LOGNAME — without them
// claude cannot read its OAuth credential and fails to authenticate with
// only a bare "exit status 1"). Nothing else crosses the boundary — no
// proxy settings, no other secrets, no OFA state.
func claudeEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TERM=dumb",
	}
	for _, k := range []string{
		"LANG",
		"USER",
		"LOGNAME",
		"XDG_CONFIG_HOME",
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
