package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

	files, problems, err := userEntries(userDir)
	if err != nil {
		return out, problems, err
	}
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

// userEntries reads the *.toml files for a user catalog, sorted by base name so
// that "first file wins" is deterministic. An absent, empty, or non-directory
// dir yields no entries and no problems: not having a user catalog is the
// normal case, not a fault worth printing. A file that cannot be read is
// reported as a problem and skipped, like a file that will not parse.
func userEntries(dir string) ([]entry, []Problem, error) {
	// filepath.Join("", "*.toml") is the bare pattern "*.toml", which would
	// glob the process's working directory. An empty dir means "none".
	if dir == "" {
		return nil, nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(matches)
	entries := make([]entry, 0, len(matches))
	var problems []Problem
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			problems = append(problems, Problem{File: filepath.Base(m), Err: err})
			continue
		}
		entries = append(entries, entry{name: filepath.Base(m), data: b})
	}
	return entries, problems, nil
}