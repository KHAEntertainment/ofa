// Package catalog defines the Catalog: the shipped knowledge of which coding
// harnesses exist and how to detect and update each. Entries are TOML files,
// one per harness, embedded in the binary and loadable from a directory.
package catalog

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"
)

//go:embed data/*.toml
var catalogFS embed.FS

// Harness is one coding harness and every install method it can be found under.
type Harness struct {
	ID       string    `toml:"id"`
	Name     string    `toml:"name"`
	Homepage string    `toml:"homepage"`
	Installs []Install `toml:"installs"`
}

// Install is a single detectable install method for a harness.
type Install struct {
	Method  string   `toml:"method"`  // brew-formula | brew-cask | npm | pipx | uv | native
	Package string   `toml:"package"` // registry name: brew formula/cask, npm package, pipx/uv package; empty for native
	Bin     string   `toml:"bin"`     // command name
	Paths   []string `toml:"paths"`   // probe paths (native only)
	Version CmdSpec  `toml:"version"` // how to read the installed version
	Latest  string   `toml:"latest"`  // self | brew | npm | pypi | none
	Update  CmdSpec  `toml:"update"`  // command to apply an update; empty = report-only
}

// CmdSpec is a command and its arguments.
type CmdSpec struct {
	Cmd []string `toml:"cmd"`
}

// validMethods lists the implementable methods. It must stay in lockstep with
// detect's registry switch (internal/detect/methods.go) and the native probe
// (internal/detect/scan.go): a method accepted here that detect does not
// implement would validate at load time and then silently detect nothing.
var validMethods = map[string]bool{
	"brew-formula": true, "brew-cask": true, "npm": true, "pipx": true, "uv": true, "native": true,
}

var validLatest = map[string]bool{
	"self": true, "brew": true, "npm": true, "pypi": true, "none": true,
}

// Embedded returns the catalog baked into the binary at build time.
func Embedded() ([]Harness, error) {
	dirEntries, err := fs.ReadDir(catalogFS, "data")
	if err != nil {
		return nil, err
	}
	entries := make([]entry, 0, len(dirEntries))
	for _, de := range dirEntries {
		b, err := fs.ReadFile(catalogFS, "data/"+de.Name())
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry{name: de.Name(), data: b})
	}
	return loadEntries(entries)
}

// Load reads every *.toml file in dir and returns the harnesses, sorted by ID.
func Load(dir string) ([]Harness, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no *.toml catalog entries in %s", dir)
	}
	entries := make([]entry, 0, len(matches))
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry{name: filepath.Base(m), data: b})
	}
	return loadEntries(entries)
}

type entry struct {
	name string
	data []byte
}

func loadEntries(entries []entry) ([]Harness, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("no catalog entries")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	var out []Harness
	seen := map[string]bool{}
	for _, e := range entries {
		var h Harness
		if _, err := toml.Decode(string(e.data), &h); err != nil {
			return nil, fmt.Errorf("%s: %w", e.name, err)
		}
		if err := h.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", e.name, err)
		}
		if seen[h.ID] {
			return nil, fmt.Errorf("%s: duplicate id %q", e.name, h.ID)
		}
		seen[h.ID] = true
		out = append(out, h)
	}
	return out, nil
}

func (h Harness) validate() error {
	if h.ID == "" {
		return fmt.Errorf("missing id")
	}
	if h.Name == "" {
		return fmt.Errorf("missing name")
	}
	if len(h.Installs) == 0 {
		return fmt.Errorf("no [[installs]] entries")
	}
	for i, in := range h.Installs {
		if !validMethods[in.Method] {
			return fmt.Errorf("install[%d]: unknown method %q", i, in.Method)
		}
		if in.Bin == "" {
			return fmt.Errorf("install[%d]: missing bin", i)
		}
		if in.Latest != "" && !validLatest[in.Latest] {
			return fmt.Errorf("install[%d]: unknown latest %q", i, in.Latest)
		}
		if len(in.Version.Cmd) == 0 {
			return fmt.Errorf("install[%d]: missing version.cmd", i)
		}
	}
	return nil
}
