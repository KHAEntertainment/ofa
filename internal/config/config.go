// Package config resolves OFA's local config directory and file paths, and
// loads the user's config file.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the user-editable configuration at [ConfigPath]. It is separate
// from state.json on purpose: state is the scan record OFA writes, config is
// intent the user writes.
type Config struct {
	// Pins lists harness ids to freeze at their installed version. OFA still
	// reports their state (marked "pinned") but never counts, offers, or
	// applies their updates.
	Pins []string `toml:"pins"`
}

// PinSet returns the pins as a lookup set, or nil when nothing is pinned. A
// nil map reads false for every id, so callers can index it unconditionally.
func (c Config) PinSet() map[string]bool {
	if len(c.Pins) == 0 {
		return nil
	}
	set := make(map[string]bool, len(c.Pins))
	for _, id := range c.Pins {
		set[id] = true
	}
	return set
}

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

// ConfigPath returns the path to the user config file.
func ConfigPath() string {
	return filepath.Join(Dir(), "config.toml")
}

// CatalogDir returns the path to the user catalog directory, where the user can
// add a harness or replace one the binary ships with. Like ConfigPath, OFA only
// ever reads it; nothing here creates it.
func CatalogDir() string {
	return filepath.Join(Dir(), "catalog")
}

// DoctorDir returns the root directory for `ofa doctor` session artifacts
// (facts, prompts, and any proposal, under history/). It is derived from Dir
// like CatalogDir, but unlike the catalog directory doctor does create it —
// only when doctor actually runs, never during scan/check/apply.
func DoctorDir() string {
	return filepath.Join(Dir(), "doctor")
}

// Load reads the user config. A missing file is an empty config, not an error:
// pinning is opt-in and most users never create the file.
//
// Unknown keys and empty ids are hard errors. A typo in a key would otherwise
// pin nothing while looking like it worked, and OFA would happily update a
// harness the user meant to freeze.
func Load() (Config, error) {
	var c Config
	b, err := os.ReadFile(ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", ConfigPath(), err)
	}

	md, err := toml.Decode(string(b), &c)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", ConfigPath(), err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		names := make([]string, 0, len(keys))
		for _, k := range keys {
			names = append(names, k.String())
		}
		return Config{}, fmt.Errorf("%s: unknown key(s): %s", ConfigPath(), strings.Join(names, ", "))
	}

	for i, id := range c.Pins {
		c.Pins[i] = strings.TrimSpace(id)
		if c.Pins[i] == "" {
			return Config{}, fmt.Errorf("%s: pins contains an empty id", ConfigPath())
		}
	}
	return c, nil
}

// EnsureDir creates the config directory if it does not exist.
func EnsureDir() error {
	return os.MkdirAll(Dir(), 0o755)
}
