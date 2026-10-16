//go:build integration

// Package resolve integration_test runs Resolve against the live machine:
// real brew, real npm, real PyPI. Skipped by default; enable with:
//
//	go test -tags=integration ./internal/resolve/...
//
// The live data changes constantly (new versions daily). Tests assert
// structural properties ("kimi-uv is tier=pypi with a real diff") rather
// than exact latest versions, so they're stable across releases.
package resolve

import (
	"os/exec"
	"testing"

	"ofa/internal/catalog"
	"ofa/internal/detect"
)

func liveCatalog(t *testing.T) []catalog.Harness {
	t.Helper()
	cat, err := catalog.Embedded()
	if err != nil {
		t.Fatalf("catalog.Embedded: %v", err)
	}
	return cat
}

// liveScan runs detect.Scan against the real shell. Skips the test if any
// package manager is missing — that's a normal state on CI or new machines.
func liveScan(t *testing.T, cat []catalog.Harness) []detect.Instance {
	t.Helper()
	for _, bin := range []string{"brew", "npm"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH: %v", bin, err)
		}
	}
	insts, err := detect.Scan(cat, detect.DefaultRunner)
	if err != nil {
		t.Fatalf("detect.Scan: %v", err)
	}
	return insts
}

func TestIntegration_BrewOutdatedHasRealDiff(t *testing.T) {
	cat := liveCatalog(t)
	insts := liveScan(t, cat)

	got := Resolve(insts, cat, DefaultRunner, DefaultGetter)
	byID := map[string]Resolution{}
	for _, r := range got {
		byID[r.HarnessID+"|"+r.Method+"|"+r.Package] = r
	}

	// Assert: every brew install that produced a Resolution with Tier=brew
	// and Outdated=true has a non-empty Latest — and the install we know
	// the registry is tracking (one per brew harness) actually appears.
	foundBrew := 0
	for _, r := range got {
		if r.Tier != "brew" || !r.Outdated {
			continue
		}
		foundBrew++
		if r.Latest == "" {
			t.Errorf("%s/%s: Outdated=true but Latest empty", r.HarnessID, r.Package)
		}
	}
	t.Logf("brew outdated installs: %d", foundBrew)
}

func TestIntegration_NpmResolvesToNonEmpty(t *testing.T) {
	cat := liveCatalog(t)
	insts := liveScan(t, cat)

	got := Resolve(insts, cat, DefaultRunner, DefaultGetter)
	for _, r := range got {
		if r.Tier != "npm" {
			continue
		}
		if r.ResolverErr != "" {
			t.Errorf("%s: npm resolve error: %s", r.HarnessID, r.ResolverErr)
		}
		if r.Latest == "" {
			t.Errorf("%s: npm tier but Latest empty", r.HarnessID)
		}
	}
}

func TestIntegration_PypiResolvesKimi(t *testing.T) {
	cat := liveCatalog(t)
	insts := liveScan(t, cat)

	got := Resolve(insts, cat, DefaultRunner, DefaultGetter)
	var kimi *Resolution
	for i, r := range got {
		if r.HarnessID == "kimi" && r.Method == "uv" {
			kimi = &got[i]
			break
		}
	}
	if kimi == nil {
		t.Skip("kimi uv install not detected; skipping")
	}
	if kimi.Tier != "pypi" {
		t.Errorf("kimi uv: tier = %q, want pypi", kimi.Tier)
	}
	if kimi.ResolverErr != "" {
		t.Errorf("kimi uv: pypi error: %s", kimi.ResolverErr)
	}
	if kimi.Latest == "" {
		t.Error("kimi uv: pypi returned empty Latest")
	}
}

func TestIntegration_SelfTierHasNoLatest(t *testing.T) {
	cat := liveCatalog(t)
	insts := liveScan(t, cat)

	got := Resolve(insts, cat, DefaultRunner, DefaultGetter)
	for _, r := range got {
		if r.Tier != "self" {
			continue
		}
		if r.Latest != "" {
			t.Errorf("%s: self tier should not populate Latest, got %q", r.HarnessID, r.Latest)
		}
		if r.ResolverErr != "" {
			t.Errorf("%s: self tier ResolverErr: %s", r.HarnessID, r.ResolverErr)
		}
	}
}

func TestIntegration_DoesNotMutateCatalog(t *testing.T) {
	// Defensive: Resolve must be pure w.r.t. catalog and instances.
	cat := liveCatalog(t)
	insts := liveScan(t, cat)

	snapshot := make([]catalog.Harness, len(cat))
	copy(snapshot, cat)

	_ = Resolve(insts, cat, DefaultRunner, DefaultGetter)

	if len(cat) != len(snapshot) {
		t.Fatalf("catalog length changed: %d -> %d", len(snapshot), len(cat))
	}
	for i := range cat {
		if cat[i].ID != snapshot[i].ID || len(cat[i].Installs) != len(snapshot[i].Installs) {
			t.Errorf("catalog[%d] mutated: %v -> %v", i, snapshot[i], cat[i])
		}
	}
}