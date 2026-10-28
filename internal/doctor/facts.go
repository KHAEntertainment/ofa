package doctor

import (
	"fmt"
	"path/filepath"
	"strings"

	"ofa/internal/detect"
)

// Fact size caps. The facts travel inside the agent prompt, so they are
// bounded on three axes — lines, line length, total bytes. Long output is
// the common case for --help, and unbounded output would both bloat the
// prompt and hand the (untrusted) binary more room for prompt injection.
const (
	versionLines = 4
	helpLines    = 12
	maxLineRunes = 240
	versionMax   = 1024
	helpMax      = 2048
	errMax       = 200
)

// Facts is everything doctor knows about one target binary, gathered before
// any agent is involved. All of it is untrusted: --version and --help output
// is whatever the binary chose to print, and the paths come from this
// machine's PATH and symlink layout.
type Facts struct {
	Bin string
	// Path is where LookPath found the bin; RealPath is the same path with
	// every symlink resolved, which reveals the install layout (npm
	// node_modules, brew Cellar, pipx venvs, uv tools) without querying
	// any registry.
	Path     string
	RealPath string
	// VersionOut / HelpOut hold truncated command output; the Err fields
	// hold the short error when the command failed instead. A failing
	// --version or --help is a fact, not an error: the agent may still
	// draft a usable entry, and the failure itself is evidence.
	VersionOut string
	VersionErr string
	HelpOut    string
	HelpErr    string
}

// CollectFacts gathers the facts for bin, already resolved to path by
// LookPath, running the binary through the same Runner seam detect uses so
// no new execution path exists here.
func CollectFacts(bin, path string, run detect.Runner) Facts {
	f := Facts{Bin: bin, Path: path, RealPath: path}
	if r, err := filepath.EvalSymlinks(path); err == nil {
		f.RealPath = r
	}
	if out, err := run(bin, "--version"); err != nil {
		f.VersionErr = truncateRunes(err.Error(), errMax)
	} else {
		f.VersionOut = firstLines(out, versionLines, versionMax)
	}
	if out, err := run(bin, "--help"); err != nil {
		f.HelpErr = truncateRunes(err.Error(), errMax)
	} else {
		f.HelpOut = firstLines(out, helpLines, helpMax)
	}
	return f
}

// String renders the facts as the plain key: value block that travels inside
// the prompt's untrusted-data section and the session's context.md.
func (f Facts) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bin: %s\n", f.Bin)
	fmt.Fprintf(&b, "path on PATH: %s\n", f.Path)
	fmt.Fprintf(&b, "real path (symlinks resolved): %s\n", f.RealPath)
	if f.VersionErr != "" {
		fmt.Fprintf(&b, "--version failed: %s\n", f.VersionErr)
	} else {
		fmt.Fprintf(&b, "--version output:\n%s\n", f.VersionOut)
	}
	if f.HelpErr != "" {
		fmt.Fprintf(&b, "--help failed: %s\n", f.HelpErr)
	} else {
		fmt.Fprintf(&b, "--help output (first %d lines):\n%s\n", helpLines, f.HelpOut)
	}
	return b.String()
}

// firstLines truncates command output to at most n lines, each at most
// maxLineRunes runes (UTF-8 boundaries respected), totaling at most maxTotal
// bytes, marking each cut so a reader knows output continued.
func firstLines(s string, n, maxTotal int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
		lines[len(lines)-1] += " …[truncated]"
	}
	total := 0
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if l := len([]rune(line)); l > maxLineRunes {
			line = truncateRunes(line, maxLineRunes) + " …[truncated]"
		}
		lines[i] = line
		total += len(line)
		if total > maxTotal {
			lines[i] += " …[truncated]"
			lines = lines[:i+1]
			break
		}
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// truncateRunes caps s at max runes, cutting on a rune boundary.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == max {
			return s[:i]
		}
		count++
	}
	return s
}
