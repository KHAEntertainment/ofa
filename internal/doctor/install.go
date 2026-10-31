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

// PublishedError reports that the catalog entry IS in place, but a step
// after publishing it failed (removing the temp name, syncing the
// directory). Callers treat it as a warning, never as a failed install:
// the file must not be deleted or rolled back.
type PublishedError struct {
	Path string
	Err  error
}

func (e *PublishedError) Error() string { return e.Err.Error() }
func (e *PublishedError) Unwrap() error { return e.Err }

// SyncDirFn makes directory-entry changes in a directory durable. It is a
// variable only so tests can inject a failure.
var SyncDirFn = syncDir

// syncDir fsyncs the directory itself: fsyncing the file does not make
// its directory entry durable, so after a power loss a published entry
// could otherwise be missing.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
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
		published = true
		if err := SyncDirFn(dir); err != nil {
			return target, &PublishedError{target, fmt.Errorf("doctor: installed to %s, but could not sync the directory: %w", target, err)}
		}
		return target, nil
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
			return target, &PublishedError{target, fmt.Errorf("doctor: installed to %s, but could not remove the temp file %s: %w", target, tmpName, err)}
		}
		published = true
		if err := SyncDirFn(dir); err != nil {
			return target, &PublishedError{target, fmt.Errorf("doctor: installed to %s, but could not sync the directory: %w", target, err)}
		}
		return target, nil
	}
}

// DuplicateIDFiles returns the base names of user catalog files, other
// than <id>.toml itself, that already declare the harness id. The user
// catalog merges files in file-name order and the first file declaring an
// id wins: a second file with the same id is reported as a problem and
// silently ignored. Installing <id>.toml while another file already
// declares that id means one of the two loses — the caller must warn and
// get an explicit second confirmation.
//
// The directory is enumerated through catalog.UserCatalogFiles — the
// merged loader's own literal listing, never a glob: a catalog directory
// name may contain glob syntax (`catalog[1]` is a name, not a class),
// and a glob would find nothing there while the loader finds the file.
// Files that fail to parse are skipped: the merge skips them the same
// way, so they cannot shadow anything.
func DuplicateIDFiles(dir, id string) []string {
	own := id + ".toml"
	var out []string
	for _, name := range catalog.UserCatalogFiles(dir) {
		if name == own {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
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
