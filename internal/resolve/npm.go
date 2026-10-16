package resolve

import (
	"fmt"
	"strings"
)

// resolveNpm asks the npm registry for the latest version of pkg. Output is
// a single version string on stdout; stderr is allowed to be noisy (npm
// warns about update-notifier etc.) and is intentionally discarded by
// DefaultRunner's Output() call.
func resolveNpm(pkg string, run Runner) (string, error) {
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