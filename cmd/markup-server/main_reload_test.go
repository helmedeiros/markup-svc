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

const initialCSV = `name,condition,factor,priority
enterprise,customer_tier == 'enterprise',1.15,10
`

const updatedCSV = `name,condition,factor,priority
enterprise,customer_tier == 'enterprise',1.42,10
`

// TestE2EReloadChangesDecisionsOverHTTP is the load-bearing test for
// ADR-0008: a successful POST /admin/reload must produce an
// observably-changed Decision on the next /decide call. The flow:
//
//   1. Write initialCSV (factor 1.15) to a tmp path.
//   2. Boot the same loader + wireHandler combo run() uses.
//   3. POST /decide and capture factor (must be 1.15).
//   4. Overwrite the CSV on disk with updatedCSV (factor 1.42).
//   5. POST /admin/reload (must be 200 with rule_count=1).
//   6. POST /decide again (must be 1.42).
//
// If the reload only swapped the response header but kept the same
// inner Decider, step 6 would still return 1.15 and the test would
// fail. The asymmetry between steps 3 and 6 is the proof.
func TestE2EReloadChangesDecisionsOverHTTP(t *testing.T) {
	rulesPath := filepath.Join(t.TempDir(), "rules.csv")
	if err := os.WriteFile(rulesPath, []byte(initialCSV), 0o644); err != nil {
		t.Fatalf("write initial CSV: %v", err)
	}

	loader := rulesLoader(rulesPath, "inmemory", "v0-reload", io.Discard)
	handler, initial, err := wireHandler(loader)
	if err != nil {
		t.Fatalf("wireHandler: %v", err)
	}
	if initial.RuleCount != 1 {
		t.Fatalf("initial RuleCount = %d, want 1", initial.RuleCount)
	}

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Step 3: pre-reload Decide returns 1.15.
	pre := decideFactor(t, srv.URL, `{"customer_tier":"enterprise"}`)
	if pre != 1.15 {
		t.Fatalf("pre-reload factor = %v, want 1.15", pre)
	}

	// Step 4: overwrite the CSV on disk.
	if err := os.WriteFile(rulesPath, []byte(updatedCSV), 0o644); err != nil {
		t.Fatalf("overwrite CSV: %v", err)
	}

	// Step 5: reload.
	reloadResp, err := http.Post(srv.URL+"/admin/reload", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /admin/reload: %v", err)
	}
	if reloadResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(reloadResp.Body)
		reloadResp.Body.Close()
		t.Fatalf("/admin/reload status = %d; body=%s", reloadResp.StatusCode, body)
	}
	var reloadBody map[string]interface{}
	_ = json.NewDecoder(reloadResp.Body).Decode(&reloadBody)
	reloadResp.Body.Close()
	if reloadBody["rule_count"].(float64) != 1 {
		t.Errorf("reload rule_count = %v, want 1", reloadBody["rule_count"])
	}
	if reloadBody["model_version"] != "v0-reload" {
		t.Errorf("reload model_version = %v, want \"v0-reload\"", reloadBody["model_version"])
	}

	// Step 6: post-reload Decide must reflect the new factor.
	post := decideFactor(t, srv.URL, `{"customer_tier":"enterprise"}`)
	if post != 1.42 {
		t.Fatalf("post-reload factor = %v, want 1.42 (reload did not change behavior)", post)
	}
}

func TestE2EReloadFailureKeepsOldDecider(t *testing.T) {
	rulesPath := filepath.Join(t.TempDir(), "rules.csv")
	if err := os.WriteFile(rulesPath, []byte(initialCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	loader := rulesLoader(rulesPath, "inmemory", "v0-reload", io.Discard)
	handler, _, err := wireHandler(loader)
	if err != nil {
		t.Fatalf("wireHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Sanity: pre-reload Decide returns 1.15.
	pre := decideFactor(t, srv.URL, `{"customer_tier":"enterprise"}`)
	if pre != 1.15 {
		t.Fatalf("pre-reload factor = %v, want 1.15", pre)
	}

	// Corrupt the CSV so reload fails at parse-time.
	if err := os.WriteFile(rulesPath, []byte("not,valid,csv\n\"unterminated"), 0o644); err != nil {
		t.Fatal(err)
	}

	reloadResp, err := http.Post(srv.URL+"/admin/reload", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /admin/reload: %v", err)
	}
	if reloadResp.StatusCode != http.StatusInternalServerError {
		body, _ := io.ReadAll(reloadResp.Body)
		reloadResp.Body.Close()
		t.Fatalf("/admin/reload status = %d, want 500; body=%s", reloadResp.StatusCode, body)
	}
	reloadResp.Body.Close()

	// Old Decider must still be active.
	post := decideFactor(t, srv.URL, `{"customer_tier":"enterprise"}`)
	if post != 1.15 {
		t.Fatalf("post-failed-reload factor = %v, want 1.15 (failed reload must not swap)", post)
	}
}

func decideFactor(t *testing.T, baseURL, body string) float64 {
	t.Helper()
	resp, err := http.Post(baseURL+"/decide", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /decide: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("/decide status = %d; body=%s", resp.StatusCode, raw)
	}
	var decision map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&decision); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return decision["markup_factor"].(float64)
}
