package detect

import (
	"encoding/json"
	"fmt"
	"strings"
)

// registryInstalled maps a registry name (formula/cask/package) to its version.
type registryInstalled map[string]string

// registryMethods is the ordered set of registry-backed install methods.
var registryMethods = []string{"brew-formula", "brew-cask", "npm", "pipx", "uv"}

// detectRegistry runs the detector for one registry method. A missing package
// manager is an empty set, not an error.
func detectRegistry(method string, run Runner) (registryInstalled, error) {
	switch method {
	case "brew-formula":
		out, err := run("brew", "list", "--versions")
		if isNotFound(err) {
			return registryInstalled{}, nil
		}
		if err != nil {
			return nil, err
		}
		return parseKeyVersion(out), nil
	case "brew-cask":
		out, err := run("brew", "list", "--cask", "--versions")
		if isNotFound(err) {
			return registryInstalled{}, nil
		}
		if err != nil {
			return nil, err
		}
		return parseKeyVersion(out), nil
	case "npm":
		out, err := run("npm", "ls", "-g", "--depth=0", "--json")
		if isNotFound(err) {
			return registryInstalled{}, nil
		}
		if err != nil {
			return nil, err
		}
		return parseNpmJSON(out)
	case "pipx":
		out, err := run("pipx", "list", "--json")
		if isNotFound(err) {
			return registryInstalled{}, nil
		}
		if err != nil {
			return nil, err
		}
		return parsePipxJSON(out)
	case "uv":
		out, err := run("uv", "tool", "list")
		if isNotFound(err) {
			return registryInstalled{}, nil
		}
		if err != nil {
			return nil, err
		}
		return parseUvList(out), nil
	}
	return nil, fmt.Errorf("unknown registry method %q", method)
}

// parseKeyVersion parses "name version ..." lines (brew list --versions output).
func parseKeyVersion(out string) registryInstalled {
	m := registryInstalled{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		m[fields[0]] = fields[1]
	}
	return m
}

func parseNpmJSON(out string) (registryInstalled, error) {
	var parsed struct {
		Dependencies map[string]struct {
			Version string `json:"version"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return nil, err
	}
	m := registryInstalled{}
	for name, dep := range parsed.Dependencies {
		m[name] = dep.Version
	}
	return m, nil
}

func parsePipxJSON(out string) (registryInstalled, error) {
	var parsed struct {
		Venvs map[string]struct {
			Metadata struct {
				MainPackage struct {
					PackageVersion string `json:"package_version"`
				} `json:"main_package"`
			} `json:"metadata"`
		} `json:"venvs"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return nil, err
	}
	m := registryInstalled{}
	for name, v := range parsed.Venvs {
		m[name] = v.Metadata.MainPackage.PackageVersion
	}
	return m, nil
}

func parseUvList(out string) registryInstalled {
	m := registryInstalled{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		m[fields[0]] = strings.TrimPrefix(fields[1], "v")
	}
	return m
}
