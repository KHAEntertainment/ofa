package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestVersionStampSurvivesBuild guards the release pipeline's version
// plumbing.
//
// The release workflow stamps the version with
// -ldflags="-X ofa/internal/cli.version=<version>". Two things can
// silently break that:
//
//  1. `version` in internal/cli being a `const` instead of a `var`.
//     -X only rewrites string variables; a const leaves the binary
//     reporting a hardcoded version no matter what tag was pushed.
//
//  2. The -X path drifting from the package that actually holds the
//     variable (e.g. a module rename). -X silently does nothing when
//     the symbol isn't found.
//
// Either failure ships a mislabeled binary — npm's package.json says
// 0.2.1 while `ofa --version` says 0.2.0 — with no error anywhere.
// So we build for real and check the output.
func TestVersionStampSurvivesBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary; skipped in -short mode")
	}

	const stamp = "9.9.9-test-stamp"

	bin := filepath.Join(t.TempDir(), "ofa")
	build := exec.Command("go", "build",
		"-ldflags=-X ofa/internal/cli.version="+stamp,
		"-o", bin,
		".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	run := exec.Command(bin, "--version")
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("running %s --version failed: %v\n%s", bin, err, out)
	}

	got := strings.TrimSpace(string(out))
	if !strings.Contains(got, stamp) {
		t.Fatalf("version stamp did not survive the build:\n  want output containing %q\n  got  %q\n\n"+
			"the release workflow stamps with -X ofa/internal/cli.version=<version>; "+
			"if that path is wrong or the target is a const, the stamp is a silent no-op",
			stamp, got)
	}
}

// TestUnstampedBuildReportsDev documents the default: a local `go build`
// with no ldflags reports "dev", not a stale release version.
func TestUnstampedBuildReportsDev(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary; skipped in -short mode")
	}

	bin := filepath.Join(t.TempDir(), "ofa")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	run := exec.Command(bin, "--version")
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("running %s --version failed: %v\n%s", bin, err, out)
	}

	got := strings.TrimSpace(string(out))
	if got != "ofa dev" {
		t.Fatalf("unstamped build should report %q, got %q", "ofa dev", got)
	}
}
