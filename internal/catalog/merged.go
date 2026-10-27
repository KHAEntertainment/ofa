package catalog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Problem is one user catalog file that could not be used. File is the base
// name of the offending file and Err says why. A problem is never fatal: the
// file is skipped and the rest of the catalog still applies.
type Problem struct {
	File string
	Err  error
}

// Error renders the problem as "file: reason", the same shape loadEntries uses
// for a hard failure, so a caller can print it and name the file to fix.
func (p Problem) Error() string {
	return fmt.Sprintf("%s: %v", p.File, p.Err)
}

// Merged returns the embedded catalog with the user catalog in userDir merged
// over it, so a user can add a harness or patch one the binary ships with
// without waiting for a release.
//
// The embedded entries load first. A user file whose id matches an embedded id
// replaces that whole entry — there is no field-level merge — and takes the
// embedded entry's position, so a user catalog cannot reorder the shipped
// output. New ids are appended in filename order. A userDir that is absent or
// holds no *.toml file therefore yields exactly Embedded()'s harnesses, in
// exactly its order.
//
// A user file that fails to parse or validate is skipped and reported as a
// Problem, as is a user file whose id an earlier user file already defined
// (first by filename order wins). Neither is an error: one stray edit must not
// cost the user the whole catalog. Duplicating an *embedded* id is not a
// problem — that is the override.
//
// The returned error covers the embedded catalog only, which nothing in
// userDir can cause. It stays separate from the problems because a corrupt
// embedded catalog is a broken binary and must fail loudly, while a bad user
// file must not.
func Merged(userDir string) ([]Harness, []Problem, error) {
	embedded, err := Embedded()
	if err != nil {
		return nil, nil, err
	}
	out := make([]Harness, len(embedded))
	copy(out, embedded)
	index := make(map[string]int, len(embedded))
	for i, h := range embedded {
		index[h.ID] = i
	}

	files, problems := userEntries(userDir)
	// fromUser records which user file defined each id, so two user files
	// claiming one id is a reported duplicate while a user file shadowing an
	// embedded id is the documented override.
	fromUser := make(map[string]string, len(files))
	for _, f := range files {
		h, err := parseEntry(f)
		if err != nil {
			problems = append(problems, Problem{File: f.name, Err: err})
			continue
		}
		if first, dup := fromUser[h.ID]; dup {
			problems = append(problems, Problem{
				File: f.name,
				Err:  fmt.Errorf("duplicate id %q, already defined in %s", h.ID, first),
			})
			continue
		}
		fromUser[h.ID] = f.name
		if i, ok := index[h.ID]; ok {
			out[i] = h
			continue
		}
		index[h.ID] = len(out)
		out = append(out, h)
	}
	return out, problems, nil
}

// userEntries reads the *.toml files for a user catalog, sorted by file name so
// that "first file wins" is deterministic. The directory is listed literally —
// never treated as a glob pattern — because it is a real path the user chose:
// `~/.config/ofa/catalog[1]` is a directory name, not a character class, and a
// pattern would either fail to match or read a different directory entirely.
//
// An absent dir, a path that is not a directory, and an empty dir all yield no
// entries and no problems: not having a user catalog is the normal case, not a
// fault worth printing. A dir that cannot be listed (permissions) is one
// problem naming the dir, and the embedded catalog still loads. A file that
// cannot be read is one problem naming that file and is skipped, like a file
// that will not parse.
func userEntries(dir string) ([]entry, []Problem) {
	// Joining "" with a name would read the process's working directory. An
	// empty dir means "no user catalog", never "the CWD".
	if dir == "" {
		return nil, nil
	}
	// Stat first so an absent path and a path that is not a directory stay
	// quiet, while a real access failure is reported against the directory.
	st, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []Problem{{File: dir, Err: err}}
	}
	if !st.IsDir() {
		return nil, nil
	}
	dirents, err := os.ReadDir(dir)
	if err != nil {
		return nil, []Problem{{File: dir, Err: err}}
	}

	names := make([]string, 0, len(dirents))
	for _, de := range dirents {
		if !strings.HasSuffix(de.Name(), ".toml") || !isCatalogFile(dir, de) {
			continue
		}
		names = append(names, de.Name())
	}
	sort.Strings(names)

	entries := make([]entry, 0, len(names))
	var problems []Problem
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			problems = append(problems, Problem{File: name, Err: err})
			continue
		}
		entries = append(entries, entry{name: name, data: b})
	}
	return entries, problems
}

// isCatalogFile reports whether de is something to read as a catalog entry: a
// regular file, or a symlink that resolves to one. A directory named *.toml is
// skipped. A symlink that cannot be resolved is kept so the read reports it as
// a problem instead of dropping it silently.
func isCatalogFile(dir string, de fs.DirEntry) bool {
	if de.Type().IsRegular() {
		return true
	}
	if de.IsDir() || de.Type()&fs.ModeSymlink == 0 {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, de.Name()))
	if err != nil {
		return true
	}
	return st.Mode().IsRegular()
}
