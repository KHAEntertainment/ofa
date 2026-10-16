package resolve

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// pypiBaseURL is the PyPI JSON API endpoint. Pulled out as a var so tests
// can redirect to a local httptest server.
var pypiBaseURL = "https://pypi.org/pypi"

// resolvePypi fetches the latest version of a package from PyPI's JSON API.
// The endpoint returns the *latest* version under info.version, regardless
// of any pre-release flag — same contract we use for npm.
func resolvePypi(pkg string, get Getter) (string, error) {
	url := fmt.Sprintf("%s/%s/json", pypiBaseURL, pkg)
	body, err := get(url)
	if err != nil {
		return "", fmt.Errorf("pypi get %s: %w", pkg, err)
	}
	var parsed struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("pypi parse %s: %w", pkg, err)
	}
	if parsed.Info.Version == "" {
		return "", fmt.Errorf("pypi %s: empty version", pkg)
	}
	return parsed.Info.Version, nil
}

// httpGetter is the production HTTP GET. Wrapped here so tests can swap in
// a fake without touching net/http.DefaultClient.
func httpGetter() Getter {
	c := &http.Client{Timeout: 10 * time.Second}
	return func(url string) ([]byte, error) {
		resp, err := c.Get(url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("pypi: %s", resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
}