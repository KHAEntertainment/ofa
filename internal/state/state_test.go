package state

import (
	"path/filepath"
	"testing"
	"time"

	"ofa/internal/detect"
)

func TestFromInstancesAndSaveLoad(t *testing.T) {
	insts := []detect.Instance{
		{HarnessID: "claude", Method: "native", Bin: "claude", Path: "/x/claude", Version: "2.1.300", Status: detect.Active},
		{HarnessID: "opencode", Method: "brew-formula", Package: "opencode", Bin: "opencode", Version: "1.19.0", Status: detect.Shadowed},
	}
	f := FromInstances(insts, time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	if f.Harnesses["claude"].Installs[0].Status != "active" {
		t.Errorf("claude status = %q", f.Harnesses["claude"].Installs[0].Status)
	}
	if f.Harnesses["opencode"].Installs[0].Status != "shadowed" {
		t.Errorf("opencode status = %q", f.Harnesses["opencode"].Installs[0].Status)
	}
	if f.Harnesses["opencode"].Installs[0].Package != "opencode" {
		t.Errorf("package = %q", f.Harnesses["opencode"].Installs[0].Package)
	}

	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	g, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if g == nil {
		t.Fatal("Load returned nil")
	}
	if g.Harnesses["claude"].Installs[0].Version != "2.1.300" {
		t.Errorf("roundtrip version = %q", g.Harnesses["claude"].Installs[0].Version)
	}
}

func TestLoadMissing(t *testing.T) {
	g, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || g != nil {
		t.Fatalf("missing file: g=%v err=%v", g, err)
	}
}

func TestDiff(t *testing.T) {
	prev := &File{Harnesses: map[string]Harness{
		"claude": {Installs: []Install{{Method: "native", Bin: "claude", Version: "2.1.300", Status: "active"}}},
		"gone":   {Installs: []Install{{Method: "native", Bin: "gone", Version: "1.0.0", Status: "active"}}},
		"qwen":   {Installs: []Install{{Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Version: "0.22.0", Status: "active"}}},
	}}
	current := &File{Harnesses: map[string]Harness{
		"claude": {Installs: []Install{{Method: "native", Bin: "claude", Version: "2.1.300", Status: "active"}}},
		"qwen":   {Installs: []Install{{Method: "npm", Package: "@qwen-code/qwen-code", Bin: "qwen", Version: "0.22.1", Status: "active"}}},
		"new":    {Installs: []Install{{Method: "npm", Package: "new-cli", Bin: "new", Version: "1.0.0", Status: "active"}}},
	}}

	d := Diff(prev, current)
	if len(d.Added) != 1 || d.Added[0].HarnessID != "new" {
		t.Errorf("added = %+v", d.Added)
	}
	if len(d.Removed) != 1 || d.Removed[0].HarnessID != "gone" {
		t.Errorf("removed = %+v", d.Removed)
	}
	if len(d.Changed) != 1 {
		t.Errorf("changed = %+v", d.Changed)
	}
	if d.Changed[0].HarnessID != "qwen" || d.Changed[0].OldVersion != "0.22.0" || d.Changed[0].NewVersion != "0.22.1" {
		t.Errorf("changed detail = %+v", d.Changed[0])
	}
}

func TestDiffNilPrev(t *testing.T) {
	current := &File{Harnesses: map[string]Harness{
		"claude": {Installs: []Install{{Method: "native", Bin: "claude", Version: "2.1.300", Status: "active"}}},
	}}
	d := Diff(nil, current)
	if len(d.Added)+len(d.Removed)+len(d.Changed) != 0 {
		t.Errorf("nil prev should yield empty diff: %+v", d)
	}
}
