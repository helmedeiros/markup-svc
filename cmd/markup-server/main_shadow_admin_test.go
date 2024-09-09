package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestE2E_ShadowAdminLifecycleLoadAndClear(t *testing.T) {
	rulesPath := filepath.Join(t.TempDir(), "rules.csv")
	if err := os.WriteFile(rulesPath, []byte(initialCSV), 0o644); err != nil {
		t.Fatalf("write initial CSV: %v", err)
	}
	loader := rulesLoader(rulesPath, "inmemory", "v0-shadow", io.Discard)
	body := newBodyLoader("inmemory", "v0-shadow", io.Discard)

	handler, _, err := wireTracedHandler(loader, body, nil, guardrailsWire{}, metricsWiring{}, nil, nil, true, 1.0, 0, "")
	if err != nil {
		t.Fatalf("wireTracedHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	challengerCSV := []byte("name,condition,factor,priority\nchallenger_rule,customer_tier == 'enterprise',1.42,99\n")
	resp, err := http.Post(srv.URL+"/admin/load-challenger", "text/csv", bytes.NewReader(challengerCSV))
	if err != nil {
		t.Fatalf("POST /admin/load-challenger: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("load-challenger status=%d body=%s", resp.StatusCode, bodyBytes)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/admin/challenger", nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /admin/challenger: %v", err)
	}
	defer func() { _ = delResp.Body.Close() }()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear-challenger status=%d", delResp.StatusCode)
	}
}

func TestE2E_ShadowAdminRoutesAbsentWhenDisabled(t *testing.T) {
	rulesPath := filepath.Join(t.TempDir(), "rules.csv")
	if err := os.WriteFile(rulesPath, []byte(initialCSV), 0o644); err != nil {
		t.Fatalf("write initial CSV: %v", err)
	}
	loader := rulesLoader(rulesPath, "inmemory", "v0-shadow", io.Discard)
	body := newBodyLoader("inmemory", "v0-shadow", io.Discard)

	handler, _, err := wireTracedHandler(loader, body, nil, guardrailsWire{}, metricsWiring{}, nil, nil, false, 1.0, 0, "")
	if err != nil {
		t.Fatalf("wireTracedHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/admin/load-challenger", "text/csv", bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("load-challenger should 404 when shadow-admin=false, got %d", resp.StatusCode)
	}
}
