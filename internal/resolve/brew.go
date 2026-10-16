package resolve

import (
	"encoding/json"
	"fmt"
	"strings"
)

// brewResolver is a single-shot cache of `brew outdated --json`. The map
// holds only outdated packages — absence means up-to-date. This is brew's
// authoritative answer and we don't need to query per-package.
type brewResolver struct {
	outdated map[string]string // name -> current_version
}

// brewOutdated is the subset of `brew outdated --json` we care about.
type brewOutdated struct {
	Formulae []brewEntry `json:"formulae"`
	Casks    []brewEntry `json:"casks"`
}

type brewEntry struct {
	Name              string   `json:"name"`
	InstalledVersions []string `json:"installed_versions"`
	CurrentVersion    string   `json:"current_version"`
}

func newBrewResolver(run Runner) (*brewResolver, error) {
	out, err := run("brew", "outdated", "--json")
	if isNotFound(err) {
		return &brewResolver{outdated: map[string]string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	parsed, err := parseBrewOutdated(out)
	if err != nil {
		return nil, fmt.Errorf("parse brew outdated: %w", err)
	}
	return parsed, nil
}

func parseBrewOutdated(out string) (*brewResolver, error) {
	var d brewOutdated
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return nil, err
	}
	r := &brewResolver{outdated: map[string]string{}}
	for _, e := range d.Formulae {
		r.outdated[e.Name] = e.CurrentVersion
	}
	for _, e := range d.Casks {
		r.outdated[e.Name] = e.CurrentVersion
	}
	return r, nil
}

// Latest returns (current_version, true) if the package is in brew's
// outdated list. Returns ("", false) if brew has no record of it being
// outdated — which is brew's authoritative "you are up to date" answer.
func (r *brewResolver) Latest(name string) (string, bool) {
	v, ok := r.outdated[name]
	return v, ok
}

// isNotFound reports whether err means brew is not on PATH. A brew that's
// installed but fails for other reasons (no JSON output, permission error)
// is still an error.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "executable file not found")
}