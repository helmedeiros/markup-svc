package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/router"
)

const routeRulesA = `name,condition,factor,priority
enterprise,customer_tier == 'enterprise',1.10,10
`
const routeRulesB = `name,condition,factor,priority
enterprise,customer_tier == 'enterprise',1.50,10
`

func writeTwoRouteCSVs(t *testing.T) (pathA, pathB string) {
	t.Helper()
	dir := t.TempDir()
	pathA = filepath.Join(dir, "route-a.csv")
	pathB = filepath.Join(dir, "route-b.csv")
	if err := os.WriteFile(pathA, []byte(routeRulesA), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, []byte(routeRulesB), 0o644); err != nil {
		t.Fatal(err)
	}
	return pathA, pathB
}

// TestE2ERouterAsymmetryOverHTTP is the load-bearing test for the
// --route + --policy cmd wiring: with two CSV routes (factor 1.10
// for v1/control, factor 1.50 for v2/treatment) and HashCorrelation
// policy, POST /decide with two distinct correlation IDs that the
// FNV-1a hash routes to different variants returns Decisions stamped
// with the route's ModelVersion + Experiment. The asymmetry is the
// proof -- without the router, both correlation IDs would hit the
// same engine and yield the same factor.
func TestE2ERouterAsymmetryOverHTTP(t *testing.T) {
	pathA, pathB := writeTwoRouteCSVs(t)

	specs := routeFlagList{
		fmt.Sprintf("v1:control:rules:%s", pathA),
		fmt.Sprintf("v2:treatment:rules:%s", pathB),
	}
	routes, total, err := buildRoutes(specs, io.Discard)
	if err != nil {
		t.Fatalf("buildRoutes: %v", err)
	}
	if total != 2 {
		t.Errorf("total rule count = %d, want 2", total)
	}
	policy, err := pickRouterPolicy("hash-correlation")
	if err != nil {
		t.Fatalf("pickRouterPolicy: %v", err)
	}
	r := router.New(routes, policy)
	srv := httptest.NewServer(wireRouterHandler(r, nil))
	t.Cleanup(srv.Close)

	// Find two correlation IDs that the hash sends to different routes
	// so the asymmetry test does not depend on a specific hash output.
	idA, idB := findDifferingCorrelationIDs(t, srv.URL)
	decA := decideRouterFactor(t, srv.URL, idA)
	decB := decideRouterFactor(t, srv.URL, idB)

	if decA.modelVersion == decB.modelVersion {
		t.Fatalf("asymmetry collapsed: both correlation IDs routed to model_version=%q", decA.modelVersion)
	}
	if decA.modelVersion != "v1" && decA.modelVersion != "v2" {
		t.Errorf("decA model_version = %q, want \"v1\" or \"v2\"", decA.modelVersion)
	}
	if decA.modelVersion == "v1" && decA.experiment != "control" {
		t.Errorf("v1 must come with experiment=control; got %q", decA.experiment)
	}
	if decA.modelVersion == "v2" && decA.experiment != "treatment" {
		t.Errorf("v2 must come with experiment=treatment; got %q", decA.experiment)
	}
	// The stamped factor should match the CSV the route was loaded from.
	if decA.modelVersion == "v1" && decA.factor != 1.10 {
		t.Errorf("v1 factor = %v, want 1.10", decA.factor)
	}
	if decA.modelVersion == "v2" && decA.factor != 1.50 {
		t.Errorf("v2 factor = %v, want 1.50", decA.factor)
	}
}

// TestE2ERouterStickyByCorrelationIDOverHTTP confirms the same
// correlation ID returns the same routing decision across many
// repeated calls -- the property dashboards need to compare an A/B
// experiment's variants like-for-like.
func TestE2ERouterStickyByCorrelationIDOverHTTP(t *testing.T) {
	pathA, pathB := writeTwoRouteCSVs(t)
	specs := routeFlagList{
		fmt.Sprintf("v1:control:rules:%s", pathA),
		fmt.Sprintf("v2:treatment:rules:%s", pathB),
	}
	routes, _, err := buildRoutes(specs, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := pickRouterPolicy("hash-correlation")
	r := router.New(routes, policy)
	srv := httptest.NewServer(wireRouterHandler(r, nil))
	t.Cleanup(srv.Close)

	first := decideRouterFactor(t, srv.URL, "trace-sticky-1")
	for i := 0; i < 50; i++ {
		got := decideRouterFactor(t, srv.URL, "trace-sticky-1")
		if got.modelVersion != first.modelVersion || got.experiment != first.experiment {
			t.Fatalf("[%d] stickiness broken: first=%s/%s got=%s/%s", i, first.modelVersion, first.experiment, got.modelVersion, got.experiment)
		}
	}
}

func TestE2ERouterReloadEndpointAbsentInRouterMode(t *testing.T) {
	pathA, pathB := writeTwoRouteCSVs(t)
	specs := routeFlagList{
		fmt.Sprintf("v1::rules:%s", pathA),
		fmt.Sprintf("v2::rules:%s", pathB),
	}
	routes, _, err := buildRoutes(specs, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	r := router.New(routes, router.DefaultPolicy{})
	srv := httptest.NewServer(wireRouterHandler(r, nil))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/admin/reload", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/admin/reload in router mode: status = %d, want 404", resp.StatusCode)
	}
}

func TestBuildRoutesRejectsMalformedSpec(t *testing.T) {
	specs := routeFlagList{"v1:variant"}
	_, _, err := buildRoutes(specs, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "want model:variant:type:path") {
		t.Fatalf("got %v, want format error", err)
	}
}

func TestBuildRoutesRejectsEmptyModel(t *testing.T) {
	specs := routeFlagList{":control:rules:r.csv"}
	_, _, err := buildRoutes(specs, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "model field is required") {
		t.Fatalf("got %v, want model-required error", err)
	}
}

func TestBuildRoutesRejectsBadSourceType(t *testing.T) {
	specs := routeFlagList{"v1:control:invalid:r.csv"}
	_, _, err := buildRoutes(specs, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "rules|snapshot") {
		t.Fatalf("got %v, want type error", err)
	}
}

func TestBuildRoutesPropagatesLoadError(t *testing.T) {
	specs := routeFlagList{"v1:control:rules:/path/does/not/exist.csv"}
	_, _, err := buildRoutes(specs, io.Discard)
	if err == nil {
		t.Fatal("want propagated load error, got nil")
	}
}

func TestPickRouterPolicyUnknownNameErrors(t *testing.T) {
	_, err := pickRouterPolicy("not-a-policy")
	if err == nil || !strings.Contains(err.Error(), "hash-correlation, default") {
		t.Fatalf("got %v, want unknown-policy error", err)
	}
}

func TestRunRouterMutuallyExclusiveWithRulesFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(),
		[]string{"--route", "v1:control:rules:x.csv", "--rules", "y.csv"},
		&stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("got %v, want mutual-exclusion error", err)
	}
}

// decideOutput captures the small subset of /decide response fields
// tests care about, decoded from the JSON wire.
type decideOutput struct {
	modelVersion string
	experiment   string
	factor       float64
}

func decideRouterFactor(t *testing.T, baseURL, correlationID string) decideOutput {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/decide", strings.NewReader(`{"customer_tier":"enterprise"}`))
	req.Header.Set("Content-Type", "application/json")
	if correlationID != "" {
		req.Header.Set("X-Correlation-ID", correlationID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	mv, _ := body["model_version"].(string)
	exp, _ := body["experiment"].(string)
	factor, _ := body["markup_factor"].(float64)
	return decideOutput{modelVersion: mv, experiment: exp, factor: factor}
}

// findDifferingCorrelationIDs probes correlation IDs until it finds
// two that the hash policy sends to different routes. The HashFNV1a
// distribution test confirms ~50/50 over 10k IDs, so the loop
// terminates quickly in practice.
func findDifferingCorrelationIDs(t *testing.T, baseURL string) (string, string) {
	t.Helper()
	first := decideRouterFactor(t, baseURL, "probe-0")
	for i := 1; i < 100; i++ {
		id := fmt.Sprintf("probe-%d", i)
		if got := decideRouterFactor(t, baseURL, id); got.modelVersion != first.modelVersion {
			return "probe-0", id
		}
	}
	t.Fatal("failed to find two correlation IDs that route to different variants in 100 attempts")
	return "", ""
}
