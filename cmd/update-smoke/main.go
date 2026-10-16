// Command update-smoke exercises the update executor against a curated
// set of safe commands. It is not part of `ofa` — it's a developer-only
// diagnostic for ticket 05.
//
// Build:  go build -o /tmp/update-smoke ./cmd/update-smoke
// Run:    /tmp/update-smoke
package main

import (
	"fmt"
	"os"
	"strings"

	"ofa/internal/update"
)

func main() {
	requests := []update.Request{
		// A guaranteed-success command.
		{
			Label: "echo (success)",
			Cmd:   update.CmdSpec{Cmd: []string{"echo", "hello from update-smoke"}},
		},
		// A guaranteed-failure command — verifies tail capture + exit-code reporting.
		{
			Label: "false (exit 1)",
			Cmd:   update.CmdSpec{Cmd: []string{"sh", "-c", "echo 'oh no'; echo 'things broke'; exit 1"}},
		},
		// A missing binary — verifies the skip-if-removed path.
		{
			Label: "missing (skip)",
			Cmd:   update.CmdSpec{Cmd: []string{"definitely-not-a-real-binary-12345"}},
		},
		// A dry-run — verifies the print path without execution.
		{
			Label:  "dry-run (print)",
			DryRun: true,
			Cmd:    update.CmdSpec{Cmd: []string{"brew", "upgrade", "--cask", "codex"}},
		},
	}

	fmt.Println("Running update-smoke (4 requests, sequential)...")
	fmt.Println()
	s := update.Apply(requests, update.DefaultExecutor)

	fmt.Println()
	fmt.Println(strings.Repeat("-", 60))
	fmt.Printf("Summary: %d succeeded, %d failed, %d skipped\n",
		len(s.Succeeded), len(s.Failed), len(s.Skipped))
	if s.HasFailures() {
		fmt.Println("FAILURES:")
		for _, f := range s.Failed {
			fmt.Printf("  - %s: exit %d, err=%v\n", f.Label, f.ExitCode, f.Err)
			if f.OutputTail != "" {
				fmt.Printf("    tail:\n")
				for _, line := range strings.Split(f.OutputTail, "\n") {
					fmt.Printf("      %s\n", line)
				}
			}
		}
	}
	if len(s.Skipped) > 0 {
		fmt.Println("SKIPPED:")
		for _, sk := range s.Skipped {
			fmt.Printf("  - %s: %s\n", sk.Label, sk.SkippedReason)
		}
	}
	os.Exit(s.ExitCode())
}