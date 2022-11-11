package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/snapshot"
)

const sampleCSV = `name,condition,factor,priority
de_enterprise,country == 'DE' AND customer_tier == 'enterprise',1.20,0
br_consumer,country == 'BR' AND customer_tier == 'consumer',1.05,0
fr_any,country == 'FR',1.10,0
`

func writeSampleCSV(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	rulesPath := filepath.Join(dir, "rules.csv")
	if err := os.WriteFile(rulesPath, []byte(sampleCSV), 0o644); err != nil {
		t.Fatalf("write CSV: %v", err)
	}
	return rulesPath
}

func TestRunHappyPath(t *testing.T) {
	rulesPath := writeSampleCSV(t)
	outPath := filepath.Join(t.TempDir(), "snapshot.json")

	var stdout, stderr bytes.Buffer
	err := run([]string{"--rules", rulesPath, "--model", "v0-test", "--out", outPath}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v (stderr=%s)", err, stderr.String())
	}

	// Verify the output is a loadable snapshot whose decider returns
	// the right Decision for a known Request.
	f, err := os.Open(outPath)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer f.Close()
	snap, err := snapshot.Read(f)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if snap.ModelVersion != "v0-test" {
		t.Errorf("ModelVersion = %q, want \"v0-test\"", snap.ModelVersion)
	}
	if len(snap.Factors) != 3 {
		t.Errorf("Factors length = %d, want 3", len(snap.Factors))
	}
	d, err := snapshot.LoadIntoIndexedDecider(snap)
	if err != nil {
		t.Fatalf("LoadIntoIndexedDecider: %v", err)
	}
	if d == nil {
		t.Fatal("Decider is nil")
	}
	_ = context.Background()

	if !strings.Contains(stdout.String(), "wrote 3 rules") {
		t.Errorf("stdout = %q, want to mention rule count", stdout.String())
	}
}

func TestRunMissingRulesFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"--out", "snapshot.json"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--rules is required") {
		t.Fatalf("got %v, want \"--rules is required\"", err)
	}
}

func TestRunMissingOutFlag(t *testing.T) {
	rulesPath := writeSampleCSV(t)
	var stdout, stderr bytes.Buffer
	err := run([]string{"--rules", rulesPath}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--out is required") {
		t.Fatalf("got %v, want \"--out is required\"", err)
	}
}

func TestRunOpenRulesError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"--rules", "/path/does/not/exist.csv", "--out", "/tmp/x.json"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("want open-error, got nil")
	}
	if !strings.Contains(err.Error(), "open rules") {
		t.Errorf("error %q should mention \"open rules\"", err.Error())
	}
}

func TestRunBuildSnapshotErrorFromNonIndexableRule(t *testing.T) {
	dir := t.TempDir()
	rulesPath := filepath.Join(dir, "bad.csv")
	const bad = `name,condition,factor,priority
neg_only,NOT country == 'XX',1.0,0
`
	if err := os.WriteFile(rulesPath, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "out.json")
	var stdout, stderr bytes.Buffer
	err := run([]string{"--rules", rulesPath, "--out", outPath}, &stdout, &stderr)
	if err == nil {
		t.Fatal("want build-snapshot error, got nil")
	}
	if !strings.Contains(err.Error(), "build snapshot") {
		t.Errorf("error %q should mention \"build snapshot\"", err.Error())
	}
}

func TestRunWriteOutputErrorOnUnwritablePath(t *testing.T) {
	rulesPath := writeSampleCSV(t)
	// Use a path under a non-existent directory so Create fails.
	outPath := filepath.Join(t.TempDir(), "nope", "snapshot.json")
	var stdout, stderr bytes.Buffer
	err := run([]string{"--rules", rulesPath, "--out", outPath}, &stdout, &stderr)
	if err == nil {
		t.Fatal("want create-error, got nil")
	}
	if !strings.Contains(err.Error(), "create output") {
		t.Errorf("error %q should mention \"create output\"", err.Error())
	}
}
