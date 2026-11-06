package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ofa/internal/detect"
)

// File is the persisted scan state.
type File struct {
	ScannedAt string             `json:"scanned_at"`
	Harnesses map[string]Harness `json:"harnesses"`
	// Latest is the resolved-latest-version cache (issue #8), keyed by
	// resolve.CacheKey ("tier:package"). Written by resolve via check/
	// apply/picker runs; carried forward by scan. Absent until the first
	// resolving run, and omitted from JSON when empty.
	Latest map[string]LatestEntry `json:"latest,omitempty"`
}

// LatestEntry is one cached latest version.
type LatestEntry struct {
	Version    string `json:"version"`
	ResolvedAt string `json:"resolved_at"` // RFC3339
}

// Harness holds the installs detected for one harness.
type Harness struct {
	Installs []Install `json:"installs"`
}

// Install is one detected install.
type Install struct {
	Method  string `json:"method"`
	Package string `json:"package,omitempty"`
	Bin     string `json:"bin"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version"`
	Status  string `json:"status"` // "active" | "shadowed"
}

// FromInstances builds a File from detection results.
func FromInstances(insts []detect.Instance, at time.Time) *File {
	f := &File{
		ScannedAt: at.UTC().Format(time.RFC3339),
		Harnesses: map[string]Harness{},
	}
	for _, i := range insts {
		h := f.Harnesses[i.HarnessID]
		h.Installs = append(h.Installs, Install{
			Method:  i.Method,
			Package: i.Package,
			Bin:     i.Bin,
			Path:    i.Path,
			Version: i.Version,
			Status:  statusString(i.Status),
		})
		f.Harnesses[i.HarnessID] = h
	}
	return f
}

func statusString(s detect.Status) string {
	if s == detect.Shadowed {
		return "shadowed"
	}
	return "active"
}

// Save writes f to path as JSON, creating the parent directory when it is
// missing.
//
// The config directory is created here, in the writing path, rather than by
// the caller: on a first run there is no ~/.config/ofa yet, and writing the
// state file into a directory that does not exist failed with "no such file
// or directory" (issue #46). Every state writer goes through Save — scan,
// the picker's first run, the latest-version cache write-back — so creating
// the parent here fixes them all at once.
//
// The mode matches config.EnsureDir: 0755, what a user gets creating the
// directory by hand.
func Save(path string, f *File) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return os.WriteFile(path, b, 0o644)
}

// Load reads the state file, returning nil if it does not exist.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return &f, nil
}
