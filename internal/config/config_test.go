package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirHonorsOFAHomeAndEnsure(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "ofa-home")
	t.Setenv("OFA_HOME", dir)

	if got := Dir(); got != dir {
		t.Fatalf("Dir() = %q, want %q", got, dir)
	}
	if got := StatePath(); got != filepath.Join(dir, "state.json") {
		t.Fatalf("StatePath() = %q", got)
	}
	if err := EnsureDir(); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("config dir not created: %v", err)
	}
	if !st.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
}
