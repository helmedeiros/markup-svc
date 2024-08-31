package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mkprom "github.com/helmedeiros/markup-svc/internal/observability/metrics/prom"
)

func TestE2E_ShadowDecideDrivesChallengerAgreementMetric(t *testing.T) {
	rulesPath := filepath.Join(t.TempDir(), "rules.csv")
	if err := os.WriteFile(rulesPath, []byte("name,condition,factor,priority\nv1_rule,customer_tier == 'enterprise',1.10,99\n"), 0o644); err != nil {
		t.Fatalf("write rules: %v", err)
	}
	loader := rulesLoader(rulesPath, "inmemory", "v0-shadow", io.Discard)
	body := newBodyLoader("inmemory", "v0-shadow", io.Discard)

	sink, shadowSink, promHandler := mkprom.New()
	mw := metricsWiring{sink: sink, shadow: shadowSink, handler: promHandler}

	handler, _, err := wireTracedHandler(loader, body, nil, guardrailsWire{}, mw, nil, nil, true, 1.0, 0)
	if err != nil {
		t.Fatalf("wireTracedHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	challengerCSV := []byte("name,condition,factor,priority\nchallenger_rule,customer_tier == 'enterprise',1.10,99\n")
	resp, err := http.Post(srv.URL+"/admin/load-challenger", "text/csv", bytes.NewReader(challengerCSV))
	if err != nil {
		t.Fatalf("load-challenger: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("load-challenger status=%d", resp.StatusCode)
	}

	decReq := []byte(`{"product_id":"p1","customer_tier":"enterprise","amount":100}`)
	for i := 0; i < 3; i++ {
		r, err := http.Post(srv.URL+"/decide", "application/json", bytes.NewReader(decReq))
		if err != nil {
			t.Fatalf("decide: %v", err)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("decide status=%d", r.StatusCode)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		promHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		got = rec.Body.String()
		if strings.Contains(got, `markup_challenger_agreement_total{agree="true"} 3`) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("never saw 3 agreements in /metrics:\n%s", got)
}

func TestE2E_ShadowDecide_DisagreementRecordsFactorDelta(t *testing.T) {
	rulesPath := filepath.Join(t.TempDir(), "rules.csv")
	if err := os.WriteFile(rulesPath, []byte("name,condition,factor,priority\nchamp,customer_tier == 'enterprise',1.10,99\n"), 0o644); err != nil {
		t.Fatalf("write rules: %v", err)
	}
	loader := rulesLoader(rulesPath, "inmemory", "v0-shadow", io.Discard)
	body := newBodyLoader("inmemory", "v0-shadow", io.Discard)

	sink, shadowSink, promHandler := mkprom.New()
	mw := metricsWiring{sink: sink, shadow: shadowSink, handler: promHandler}

	handler, _, err := wireTracedHandler(loader, body, nil, guardrailsWire{}, mw, nil, nil, true, 1.0, 0)
	if err != nil {
		t.Fatalf("wireTracedHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	chCSV := []byte("name,condition,factor,priority\nchallenger_rule,customer_tier == 'enterprise',1.40,99\n")
	resp, _ := http.Post(srv.URL+"/admin/load-challenger", "text/csv", bytes.NewReader(chCSV))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("load-challenger status=%d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	decReq := []byte(`{"product_id":"p1","customer_tier":"enterprise","amount":100}`)
	r, err := http.Post(srv.URL+"/decide", "application/json", bytes.NewReader(decReq))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	var decisionBody struct {
		MarkupFactor float64 `json:"markup_factor"`
	}
	_ = json.NewDecoder(r.Body).Decode(&decisionBody)
	_ = r.Body.Close()
	if decisionBody.MarkupFactor != 1.10 {
		t.Fatalf("response should carry champion factor 1.10, got %v", decisionBody.MarkupFactor)
	}

	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		promHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		got = rec.Body.String()
		if strings.Contains(got, `markup_challenger_agreement_total{agree="false"} 1`) &&
			strings.Contains(got, `markup_challenger_factor_delta_count 1`) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("disagreement metrics not observed:\n%s", got)
}
