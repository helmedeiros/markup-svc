// Package harness runs the scientific/v0.1.3 benchmarks measuring the
// per-Decide overhead added by the guardrails decorator from ADR-0014.
// See REPORT.md for the pre-registered bars + measurements.
package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/load"
)

func fixturePath(t testing.TB) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	return filepath.Join(wd, "fixture.csv")
}

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

func TestFixtureLoads(t *testing.T) {
	rules := loadFixture(t)
	if len(rules) == 0 {
		t.Fatal("loadFixture returned zero rules; fixture or loader is broken")
	}
}
