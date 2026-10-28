package config

import (
	"os"
	"path/filepath"
	"strings"
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
	if got := CatalogDir(); got != filepath.Join(dir, "catalog") {
		t.Fatalf("CatalogDir() = %q, want the catalog dir under OFA_HOME", got)
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

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("OFA_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return dir
}

func TestLoadMissingConfigIsEmpty(t *testing.T) {
	t.Setenv("OFA_HOME", t.TempDir())
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Pins) != 0 || c.PinSet() != nil {
		t.Errorf("missing config should yield no pins, got %+v", c)
	}
}

func TestLoadPins(t *testing.T) {
	writeConfig(t, `pins = ["opencode", " qwen "]`)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	set := c.PinSet()
	if !set["opencode"] || !set["qwen"] {
		t.Errorf("PinSet = %v, want opencode and qwen", set)
	}
	if set["claude"] {
		t.Errorf("PinSet unexpectedly contains claude: %v", set)
	}
}

func TestLoadConfigPathHonorsOFAHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OFA_HOME", dir)
	if got, want := ConfigPath(), filepath.Join(dir, "config.toml"); got != want {
		t.Errorf("ConfigPath() = %q, want %q", got, want)
	}
}

func TestLoadUnknownKeyIsError(t *testing.T) {
	writeConfig(t, `pin = ["opencode"]`)
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for an unknown key")
	}
}

func TestLoadEmptyPinIsError(t *testing.T) {
	writeConfig(t, `pins = [""]`)
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for an empty pin id")
	}
}

func TestDoctorDirHonorsOFAHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OFA_HOME", dir)
	if got, want := DoctorDir(), filepath.Join(dir, "doctor"); got != want {
		t.Errorf("DoctorDir() = %q, want %q", got, want)
	}
}

func TestDoctorDirDefaultUnderConfigDir(t *testing.T) {
	t.Setenv("OFA_HOME", "")
	got := DoctorDir()
	if want := filepath.Join(Dir(), "doctor"); got != want {
		t.Errorf("DoctorDir() = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, filepath.Join(".config", "ofa", "doctor")) {
		t.Errorf("DoctorDir() = %q, want it under ~/.config/ofa", got)
	}
}
