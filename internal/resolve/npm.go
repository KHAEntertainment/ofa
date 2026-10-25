package resolve

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// defaultNpmRegistry is npm's public registry — the only registry OFA
// queries directly over HTTP. Everything else goes through `npm view`,
// because only npm can apply the user's own configuration (auth, mirrors,
// proxies) for a custom registry.
const defaultNpmRegistry = "https://registry.npmjs.org"

// npmRegistry describes where npm packages resolve. Custom is true when
// the user's npm configuration points somewhere other than the public
// registry, which flips resolution to the spawning path.
type npmRegistry struct {
	Base   string
	Custom bool
}

// detectNpmRegistry reads the same configuration npm would, in npm's own
// precedence order: environment, project .npmrc, then user .npmrc.
//
// The global-prefix .npmrc (e.g. /opt/homebrew/etc/npmrc) is not read:
// it is rare, and a mirror configured only there still serves the public
// package versions OFA compares against. A registry configured anywhere
// we do read takes the spawn path, so correctness degrades to the old
// behavior, never to a wrong registry.
func detectNpmRegistry() npmRegistry {
	if v := firstEnv("npm_config_registry", "NPM_CONFIG_REGISTRY"); v != "" {
		return classifyRegistry(v)
	}
	for _, path := range []string{".npmrc", userNpmrcPath()} {
		if v, ok := npmrcRegistry(path); ok {
			return classifyRegistry(v)
		}
	}
	return npmRegistry{Base: defaultNpmRegistry}
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func classifyRegistry(v string) npmRegistry {
	base := strings.TrimRight(strings.TrimSpace(v), "/")
	if base == defaultNpmRegistry || base == "" {
		return npmRegistry{Base: defaultNpmRegistry}
	}
	return npmRegistry{Base: base, Custom: true}
}

// npmrcRegistry reads the `registry = ...` line from an .npmrc file,
// returning ok=false when the file is missing or has no registry line.
func npmrcRegistry(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "registry" {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if val != "" {
			return val, true
		}
	}
	return "", false
}

func userNpmrcPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".npmrc")
}

// resolveNpm returns pkg's latest version. Against the public registry
// it is a direct HTTP GET (issue #8: no npm process spawn per package —
// seven spawns cost 7–14s serially); a custom registry falls back to
// `npm view` so npm applies its own config.
func resolveNpm(pkg string, run Runner, get Getter, reg npmRegistry) (string, error) {
	if reg.Custom {
		return resolveNpmSpawn(pkg, run)
	}
	// Scope separator escaped so scoped names stay one path segment
	// (@scope%2Fname); "@" itself is left as-is, matching npm's URLs.
	url := reg.Base + "/" + strings.ReplaceAll(pkg, "/", "%2F") + "/latest"
	body, err := get(url)
	if err != nil {
		return "", fmt.Errorf("npm get %s: %w", pkg, err)
	}
	var parsed struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("npm parse %s: %w", pkg, err)
	}
	if parsed.Version == "" {
		// Registry errors (404 etc.) come back as JSON without a
		// version field; DefaultGetter doesn't check status, so this
		// is where a miss surfaces.
		return "", fmt.Errorf("npm %s: empty version", pkg)
	}
	return parsed.Version, nil
}

// resolveNpmSpawn is the pre-#8 path: one `npm view` per package. Kept
// for custom-registry resolution and as the tested fallback contract.
func resolveNpmSpawn(pkg string, run Runner) (string, error) {
	out, err := run("npm", "view", pkg, "version")
	if err != nil {
		return "", fmt.Errorf("npm view %s: %w", pkg, err)
	}
	v := strings.TrimSpace(out)
	if v == "" {
		return "", fmt.Errorf("npm view %s: empty version", pkg)
	}
	return v, nil
}
