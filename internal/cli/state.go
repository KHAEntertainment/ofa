package cli

import (
	"ofa/internal/state"
)

// stateFile is a thin alias so the rest of the cli package can talk about
// "the state file" without importing state everywhere. Kept as its own
// file because multiple commands need it.
type stateFile = state.File

// loadStateFile reads the on-disk state. Returns (nil, nil) when the file
// is missing — that's the initial-scan case, not an error.
func loadStateFile(path string) (*stateFile, error) {
	return state.Load(path)
}

// saveStateFile writes the on-disk state.
func saveStateFile(path string, f *stateFile) error {
	return state.Save(path, f)
}