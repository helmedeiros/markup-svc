package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EBootsWithBodyLoader_EmptyBodyReloadUnchanged is the bit-for-bit
// canary at the cmd level. With the body-loader wired (matching what
// run() does for every binary built from v0.1.19+), an empty-body
// POST /admin/reload still flows through the file-based loader and
// returns the same JSON response shape v0.1.18 produced. The body-loader
// presence does not change the empty-body path's behaviour. See ADR-0030.
func TestE2EBootsWithBodyLoader_EmptyBodyReloadUnchanged(t *testing.T) {
	rulesPath := filepath.Join(t.TempDir(), "rules.csv")
	if err := os.WriteFile(rulesPath, []byte(initialCSV), 0o644); err != nil {
		t.Fatalf("write initial CSV: %v", err)
	}

	loader := rulesLoader(rulesPath, "inmemory", "v0-body", io.Discard)
	body := newBodyLoader("inmemory", "v0-body", io.Discard)
	handler, _, err := wireTracedHandler(loader, body, nil, guardrailsWire{}, metricsWiring{}, nil, nil, false, 1.0, 0)
	if err != nil {
		t.Fatalf("wireTracedHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/admin/reload", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /admin/reload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		RuleCount    int    `json:"rule_count"`
		ModelVersion string `json:"model_version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RuleCount != 1 {
		t.Errorf("rule_count = %d, want 1", got.RuleCount)
	}
	if got.ModelVersion != "v0-body" {
		t.Errorf("model_version = %q, want v0-body", got.ModelVersion)
	}
}

// TestE2EBodyBasedReload_CSVHappyPath drives the body-based reload
// path end-to-end. Boot with one rule (factor 1.15); POST a different
// CSV in the body with Content-Type text/csv; then POST /decide and
// assert the new factor serves. The asymmetry between pre-reload and
// post-reload responses is the proof the body-based path actually
// swapped the Decider. See ADR-0030.
func TestE2EBodyBasedReload_CSVHappyPath(t *testing.T) {
	rulesPath := filepath.Join(t.TempDir(), "rules.csv")
	if err := os.WriteFile(rulesPath, []byte(initialCSV), 0o644); err != nil {
		t.Fatalf("write initial CSV: %v", err)
	}

	loader := rulesLoader(rulesPath, "inmemory", "v0-body", io.Discard)
	body := newBodyLoader("inmemory", "v0-body", io.Discard)
	handler, _, err := wireTracedHandler(loader, body, nil, guardrailsWire{}, metricsWiring{}, nil, nil, false, 1.0, 0)
	if err != nil {
		t.Fatalf("wireTracedHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	pre := decideFactor(t, srv.URL, `{"customer_tier":"enterprise"}`)
	if pre != 1.15 {
		t.Fatalf("pre-reload factor = %v, want 1.15", pre)
	}

	resp, err := http.Post(srv.URL+"/admin/reload", "text/csv", strings.NewReader(updatedCSV))
	if err != nil {
		t.Fatalf("POST /admin/reload: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()

	post := decideFactor(t, srv.URL, `{"customer_tier":"enterprise"}`)
	if post != 1.42 {
		t.Errorf("post-body-reload factor = %v, want 1.42 (body-based reload should swap the Decider)", post)
	}
}
