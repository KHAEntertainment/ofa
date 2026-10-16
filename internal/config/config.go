// Package config resolves OFA's local config directory and file paths.
package config

import (
	"os"
	"path/filepath"
)

// Dir returns the OFA config directory. Defaults to ~/.config/ofa (the
// convention this machine already uses for claude-mem/opencode), overridden by
// OFA_HOME. Note this diverges from os.UserConfigDir on macOS on purpose.
func Dir() string {
	if d := os.Getenv("OFA_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "ofa")
	}
	return filepath.Join(home, ".config", "ofa")
}

// StatePath returns the path to the persisted scan state file.
func StatePath() string {
	return filepath.Join(Dir(), "state.json")
}

// EnsureDir creates the config directory if it does not exist.
func EnsureDir() error {
	return os.MkdirAll(Dir(), 0o755)
}
