// Package harness runs the scientific/v0.1.0 benchmarks. See
// ADR-0012 for the methodology and REPORT.md for the pre-registered
// bars + measurements.
package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/load"
)

// fixturePath finds the rule-set fixture relative to the test binary's
// package directory so the same harness file works whether invoked
// from the repo root (`go test ./scientific/...`) or from inside the
// Docker image (which copies the repo as a unit).
func fixturePath(t testing.TB) string {
	t.Helper()
	// The test binary runs from this package's directory.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	return filepath.Join(wd, "fixture.csv")
}

// loadFixture is the shared fixture loader. Called from every benchmark
// before ResetTimer so parse cost does not count toward the
// steady-state measurement.
func loadFixture(t testing.TB) []load.Rule {
	t.Helper()
	f, err := os.Open(fixturePath(t))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	rules, err := load.FromCSV(f)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return rules
}

// TestFixtureLoads is the harness's own smoke test: confirms the
// fixture parses cleanly so a malformed CSV cannot silently produce
// zero-iteration benchmarks. Runs as part of `go test ./scientific/...`
// so the standard CI pass surfaces any breakage to the fixture or
// the loader.
func TestFixtureLoads(t *testing.T) {
	rules := loadFixture(t)
	if len(rules) != 50 {
		t.Errorf("fixture rule count = %d, want 50", len(rules))
	}
}
