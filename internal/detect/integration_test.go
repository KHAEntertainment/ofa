package detect

import (
	"os"
	"testing"

	"ofa/internal/catalog"
)

// TestScanRealMachine runs the full scan against the real machine. Skipped
// unless OFA_INTEGRATION=1, because it depends on this machine's installed set.
func TestScanRealMachine(t *testing.T) {
	if os.Getenv("OFA_INTEGRATION") == "" {
		t.Skip("set OFA_INTEGRATION=1 to run against the real machine")
	}
	hs, err := catalog.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	insts, err := Scan(hs, DefaultRunner)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	seen := map[string]bool{}
	for _, i := range insts {
		seen[i.HarnessID] = true
		t.Logf("%-14s %-13s %-22s %-10s %-10s st=%d %s", i.HarnessID, i.Method, i.Package, i.Bin, i.Version, i.Status, i.Path)
	}
	if len(seen) != 13 {
		t.Errorf("want 13 harnesses, saw %d: %v", len(seen), seen)
	}

	assertStatus(t, insts, "opencode", "npm", "opencode-ai", Active)
	assertStatus(t, insts, "opencode", "brew-formula", "opencode", Shadowed)
	assertStatus(t, insts, "qwen", "npm", "@qwen-code/qwen-code", Active)
	assertStatus(t, insts, "qwen", "brew-formula", "qwen-code", Shadowed)
}

func assertStatus(t *testing.T, insts []Instance, harness, method, pkg string, want Status) {
	t.Helper()
	for _, i := range insts {
		if i.HarnessID == harness && i.Method == method && i.Package == pkg {
			if i.Status != want {
				t.Errorf("%s/%s status = %d, want %d", harness, pkg, i.Status, want)
			}
			return
		}
	}
	t.Errorf("no instance for %s %s %s", harness, method, pkg)
}
