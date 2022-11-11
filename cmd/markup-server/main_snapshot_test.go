package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/snapshot"
)

func writeSnapshot(t *testing.T, modelVersion string) string {
	t.Helper()
	rules := loadTestRules(t)
	snap, err := snapshot.Build(rules, modelVersion)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create snapshot file: %v", err)
	}
	defer f.Close()
	if err := snapshot.Write(f, snap); err != nil {
		t.Fatalf("snapshot.Write: %v", err)
	}
	return path
}

// TestE2ESnapshotPathOverHTTP confirms ADR-0007 end-to-end through the
// cmd's wiring: a snapshot written by snapshot.Build/Write reloads via
// handlerFromSnapshot into the same http.Handler tree (Decide handler
// behind the correlation-ID middleware), and POST /decide returns a
// Decision stamped with the snapshot's ModelVersion and the indexed
// engine adapter slice.
func TestE2ESnapshotPathOverHTTP(t *testing.T) {
	path := writeSnapshot(t, "v0-snap")
	handler, ruleCount, modelVersion, err := handlerFromSnapshot(path)
	if err != nil {
		t.Fatalf("handlerFromSnapshot: %v", err)
	}
	if ruleCount != 3 {
		t.Errorf("ruleCount = %d, want 3", ruleCount)
	}
	if modelVersion != "v0-snap" {
		t.Errorf("modelVersion = %q, want \"v0-snap\"", modelVersion)
	}

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/decide", "application/json",
		strings.NewReader(`{"customer_tier":"enterprise"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["rule"] != "enterprise" {
		t.Errorf("rule = %v, want \"enterprise\"", body["rule"])
	}
	if body["markup_factor"].(float64) != 1.15 {
		t.Errorf("markup_factor = %v, want 1.15", body["markup_factor"])
	}
	if body["model_version"] != "v0-snap" {
		t.Errorf("model_version = %v, want \"v0-snap\" (from snapshot, not flag default)", body["model_version"])
	}
	if body["engine_adapter"] != "*indexed.Engine" {
		t.Errorf("engine_adapter = %v, want \"*indexed.Engine\"", body["engine_adapter"])
	}
}

func TestHandlerFromSnapshotRejectsMissingFile(t *testing.T) {
	_, _, _, err := handlerFromSnapshot(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatal("want open error, got nil")
	}
	if !strings.Contains(err.Error(), "open snapshot") {
		t.Errorf("error %q should mention \"open snapshot\"", err.Error())
	}
}

func TestHandlerFromSnapshotRejectsMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := handlerFromSnapshot(path)
	if err == nil {
		t.Fatal("want read error, got nil")
	}
	if !strings.Contains(err.Error(), "read snapshot") {
		t.Errorf("error %q should mention \"read snapshot\"", err.Error())
	}
}

func TestRunRejectsBothFlagsSet(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(),
		[]string{"--rules", "x.csv", "--snapshot", "y.json"},
		&stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("got %v, want mutually-exclusive error", err)
	}
}

func TestRunRejectsNeitherFlagSet(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("got %v, want \"required\" error", err)
	}
}
