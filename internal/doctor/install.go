package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"ofa/internal/catalog"
)

// Installing a proposal into the user catalog. The write path is the one
// place doctor leaves its own history directory, so every guarantee here
// is structural: the filename comes only from a validated id (no path
// the proposal chose can escape the catalog directory), the file appears
// atomically (a temp file in the same directory, fsynced, then linked or
// renamed into place), and a failure at any step leaves no partial file.

// idPattern is the shape of a harness id that is safe to derive a catalog
// file name from: lowercase start (no leading dot, no dash), then letters,
// digits, dots, underscores, dashes. No separators, so no traversal; no
// leading dot, so no hidden file.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// maxIDLen bounds a valid id. A longer one is almost certainly an
// accident, not a name, and never worth a filename.
const maxIDLen = 64

// ValidID reports whether id is safe to derive a catalog file name from.
// The catalogue file will be <id>.toml inside the catalog directory; every
// character class excluded here is excluded so that derivation cannot
// produce a path outside that directory.
func ValidID(id string) bool {
	return len(id) > 0 && len(id) <= maxIDLen && idPattern.MatchString(id)
}

// ErrExists reports that the target catalog file already exists and the
// caller did not authorize an overwrite. The existing file is untouched.
var ErrExists = errors.New("doctor: catalog file already exists")

// WriteCatalogEntry installs tomlSrc (the exact, already-validated bytes of
// the proposal) as <dir>/<id>.toml. The write is atomic: the bytes land in
// a temp file in the same directory, are fsynced, and are then published —
// by hard link when overwrite is false, which fails with ErrExists if the
// target already exists (a stat-then-rename would race; rename alone would
// silently replace), or by rename when the caller has confirmed an
// overwrite. Any failure removes the temp file and leaves no partial file.
//
// The file is 0600, like everything doctor writes: a native install's
// probe paths can name the user's home directory layout, which is not
// world-readable business. The catalog directory is created 0755 if
// missing, matching where a user would put it by hand.
func WriteCatalogEntry(id string, tomlSrc []byte, dir string, overwrite bool) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("doctor: refusing to install: harness id %q is not a valid catalog file name", id)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("doctor: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".doctor-install-*.tmp")
	if err != nil {
		return "", fmt.Errorf("doctor: %w", err)
	}
	tmpName := tmp.Name()
	published := false
	defer func() {
		if !published {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(tomlSrc); err != nil {
		return "", fmt.Errorf("doctor: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return "", fmt.Errorf("doctor: %w", err)
	}
	// fsync before publishing: after the link/rename returns, the bytes
	// must be on disk even if the machine dies before the next fsync
	// point.
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("doctor: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("doctor: %w", err)
	}
	target := filepath.Join(dir, id+".toml")
	if overwrite {
		if err := os.Rename(tmpName, target); err != nil {
			return "", fmt.Errorf("doctor: %w", err)
		}
	} else {
		// Link fails atomically when target exists; rename would
		// silently replace it. The temp file's content is the published
		// content (one inode, two names), so removing the temp name
		// afterwards leaves the file itself intact.
		if err := os.Link(tmpName, target); err != nil {
			if errors.Is(err, os.ErrExist) {
				return "", fmt.Errorf("%w: %s", ErrExists, target)
			}
			return "", fmt.Errorf("doctor: %w", err)
		}
		if err := os.Remove(tmpName); err != nil {
			// The entry is already published; a leftover .tmp file is
			// cosmetic. Report it so the user can clean up knowingly.
			return target, fmt.Errorf("doctor: installed to %s, but could not remove the temp file %s: %w", target, tmpName, err)
		}
		published = true
		return target, nil
	}
	published = true
	return target, nil
}

// DuplicateIDFiles returns the base names of user catalog files, other
// than <id>.toml itself, that already declare the harness id. The user
// catalog merges files in file-name order and the first file declaring an
// id wins: a second file with the same id is reported as a problem and
// silently ignored. Installing <id>.toml while another file already
// declares that id means one of the two loses — the caller must warn and
// get an explicit second confirmation.
//
// Files that fail to parse are skipped: the merge skips them the same
// way, so they cannot shadow anything.
func DuplicateIDFiles(dir, id string) []string {
	matches, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil {
		return nil
	}
	own := id + ".toml"
	var out []string
	for _, m := range matches {
		name := filepath.Base(m)
		if name == own {
			continue
		}
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		h, err := catalog.ParseEntry(name, b)
		if err != nil {
			continue
		}
		if h.ID == id {
			out = append(out, name)
		}
	}
	return out
}
