package state

import (
	"encoding/json"
	"errors"
	"os"
	"time"

	"ofa/internal/detect"
)

// File is the persisted scan state.
type File struct {
	ScannedAt string             `json:"scanned_at"`
	Harnesses map[string]Harness `json:"harnesses"`
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

// Save writes f to path as JSON.
func Save(path string, f *File) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
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
