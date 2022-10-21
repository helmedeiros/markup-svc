package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/load"
)

func loadTestRules(t *testing.T) []load.Rule {
	t.Helper()
	f, err := os.Open("testdata/rules.csv")
	if err != nil {
		t.Fatalf("open testdata/rules.csv: %v", err)
	}
	defer f.Close()
	rules, err := load.FromCSV(f)
	if err != nil {
		t.Fatalf("load.FromCSV: %v", err)
	}
	return rules
}

func newE2EServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newE2EServerWith(t, "inmemory")
}

func newE2EServerWith(t *testing.T, adapter string) *httptest.Server {
	t.Helper()
	handler, err := buildHandler(adapter, loadTestRules(t), "v0-e2e")
	if err != nil {
		t.Fatalf("buildHandler(%q): %v", adapter, err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestE2EEnterpriseTierReturns200WithDecision(t *testing.T) {
	srv := newE2EServer(t)
	resp, err := http.Post(srv.URL+"/decide", "application/json",
		strings.NewReader(`{"customer_tier":"enterprise"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body=%s), want 200", resp.StatusCode, body)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["rule"] != "enterprise" {
		t.Errorf("rule = %v, want \"enterprise\"", body["rule"])
	}
	if body["markup_factor"].(float64) != 1.15 {
		t.Errorf("markup_factor = %v, want 1.15", body["markup_factor"])
	}
	if body["model_version"] != "v0-e2e" {
		t.Errorf("model_version = %v, want \"v0-e2e\"", body["model_version"])
	}
}

func TestE2EBrazilPeakAndConditionReturns200(t *testing.T) {
	srv := newE2EServer(t)
	resp, err := http.Post(srv.URL+"/decide", "application/json",
		strings.NewReader(`{"country":"BR","time_window":"peak"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["rule"] != "br_peak" {
		t.Errorf("rule = %v, want \"br_peak\"", body["rule"])
	}
}

func TestE2ENoMatchReturns404(t *testing.T) {
	srv := newE2EServer(t)
	resp, err := http.Post(srv.URL+"/decide", "application/json",
		strings.NewReader(`{"customer_tier":"platform","country":"DE","time_window":"off"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body=%s), want 404", resp.StatusCode, body)
	}
}

func TestE2EGetReturns405WithAllowHeader(t *testing.T) {
	srv := newE2EServer(t)
	resp, err := http.Get(srv.URL + "/decide")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want POST", got)
	}
}

func TestE2ECorrelationIDSuppliedEchoesBack(t *testing.T) {
	srv := newE2EServer(t)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/decide",
		strings.NewReader(`{"customer_tier":"enterprise"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(httpapi.CorrelationIDHeader, "e2e-trace-42")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get(httpapi.CorrelationIDHeader); got != "e2e-trace-42" {
		t.Errorf("response %s = %q, want \"e2e-trace-42\"", httpapi.CorrelationIDHeader, got)
	}
	var body map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["correlation_id"] != "e2e-trace-42" {
		t.Errorf("decision correlation_id = %v, want \"e2e-trace-42\"", body["correlation_id"])
	}
}

func TestE2ECorrelationIDAbsentGeneratesAndEchoes(t *testing.T) {
	srv := newE2EServer(t)
	resp, err := http.Post(srv.URL+"/decide", "application/json",
		strings.NewReader(`{"customer_tier":"enterprise"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	got := resp.Header.Get(httpapi.CorrelationIDHeader)
	if got == "" {
		t.Fatal("expected generated correlation ID echoed on response header")
	}
	var body map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["correlation_id"] != got {
		t.Errorf("decision correlation_id = %v, response header = %q; should match", body["correlation_id"], got)
	}
}

func TestE2EMalformedJSONReturns400(t *testing.T) {
	srv := newE2EServer(t)
	resp, err := http.Post(srv.URL+"/decide", "application/json",
		strings.NewReader(`{this is not valid json`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestBuildHandlerPropagatesInmemoryError(t *testing.T) {
	rules := loadTestRules(t)
	rules = append(rules, load.Rule{Name: rules[0].Name, Condition: rules[0].Condition, Factor: 1.0, Priority: 0})
	_, err := buildHandler("inmemory", rules, "v0-e2e")
	if err == nil {
		t.Fatal("want duplicate-rule error from inmemory.NewFromRules, got nil")
	}
}

func TestBuildHandlerUnknownAdapterError(t *testing.T) {
	_, err := buildHandler("not-a-real-adapter", loadTestRules(t), "v0-e2e")
	if err == nil {
		t.Fatal("want error for unknown adapter, got nil")
	}
	if !strings.Contains(err.Error(), "not-a-real-adapter") {
		t.Errorf("error %q should name the bad adapter", err.Error())
	}
}

// TestE2EAdapterSemanticDivergenceOverHTTP is the load-bearing test
// for the --adapter flag: the same Request through the same CSV via
// the HTTP wire returns different Decisions when --adapter switches
// between inmemory and firstmatch. Three-way assertion mirrors the
// ADR-0004 contract.
func TestE2EAdapterSemanticDivergenceOverHTTP(t *testing.T) {
	const body = `{"customer_tier":"enterprise","country":"BR","time_window":"peak"}`

	inmemorySrv := newE2EServerWith(t, "inmemory")
	imResp, err := http.Post(inmemorySrv.URL+"/decide", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("inmemory POST: %v", err)
	}
	defer imResp.Body.Close()
	var imBody map[string]interface{}
	_ = json.NewDecoder(imResp.Body).Decode(&imBody)

	firstmatchSrv := newE2EServerWith(t, "firstmatch")
	fmResp, err := http.Post(firstmatchSrv.URL+"/decide", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("firstmatch POST: %v", err)
	}
	defer fmResp.Body.Close()
	var fmBody map[string]interface{}
	_ = json.NewDecoder(fmResp.Body).Decode(&fmBody)

	if imBody["rule"] == fmBody["rule"] {
		t.Fatalf("expected adapter divergence: both picked rule=%v (inmemory=%+v firstmatch=%+v)",
			imBody["rule"], imBody, fmBody)
	}
	if imBody["engine_adapter"] == fmBody["engine_adapter"] {
		t.Errorf("engine_adapter should differ between adapters: both = %v", imBody["engine_adapter"])
	}
	if fmBody["rule"] != "enterprise" {
		t.Errorf("firstmatch picked %v, want \"enterprise\" (first in CSV)", fmBody["rule"])
	}
	if imBody["rule"] != "br_peak" {
		t.Errorf("inmemory picked %v, want \"br_peak\" (last matching action)", imBody["rule"])
	}
}
