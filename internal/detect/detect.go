package detect

import (
	"errors"
	"os/exec"
)

// Status marks whether an install's binary is the live one on PATH.
type Status int

const (
	Active Status = iota
	Shadowed
)

// Instance is one detected install of a harness.
type Instance struct {
	HarnessID string
	Method    string // brew-formula | brew-cask | npm | pipx | uv | native
	Package   string // registry name (formula/cask/npm/pipx/uv package); empty for native
	Bin       string
	Path      string // resolved live path when Active; empty when Shadowed
	Version   string
	Status    Status
	// OffPath reports that this install's bin does not resolve on PATH. The
	// install is still real, but a native `<bin> update` cannot run, so
	// reachability must not be claimed for it.
	OffPath bool
}

// Runner runs a command and returns its stdout (stderr discarded). Swappable
// in tests; DefaultRunner shells out for real.
type Runner func(name string, args ...string) (string, error)

// DefaultRunner executes name with args and returns stdout.
func DefaultRunner(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

// isNotFound reports whether err means the command itself is not on PATH.
func isNotFound(err error) bool {
	var e *exec.Error
	return err != nil && errors.As(err, &e) && errors.Is(e.Err, exec.ErrNotFound)
}
