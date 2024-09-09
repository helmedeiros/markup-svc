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
	"github.com/helmedeiros/markup-svc/internal/markup"
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
	routes, holders, total, err := buildRoutes(specs, io.Discard)
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
	srv := httptest.NewServer(wireRouterHandler(r, nil, holders, guardrailsWire{}, metricsWiring{}, nil, ""))
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
	routes, holders, _, err := buildRoutes(specs, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := pickRouterPolicy("hash-correlation")
	r := router.New(routes, policy)
	srv := httptest.NewServer(wireRouterHandler(r, nil, holders, guardrailsWire{}, metricsWiring{}, nil, ""))
	t.Cleanup(srv.Close)

	first := decideRouterFactor(t, srv.URL, "trace-sticky-1")
	for i := 0; i < 50; i++ {
		got := decideRouterFactor(t, srv.URL, "trace-sticky-1")
		if got.modelVersion != first.modelVersion || got.experiment != first.experiment {
			t.Fatalf("[%d] stickiness broken: first=%s/%s got=%s/%s", i, first.modelVersion, first.experiment, got.modelVersion, got.experiment)
		}
	}
}

// TestE2ERouterReloadWithoutHoldersReturns404 confirms a Router-mode
// handler built without per-route holders returns 404 on
// /admin/reload (the legacy posture before this commit added per-route
// reload). Holders-mode behaviour is covered by
// TestE2ERouterPerRouteReloadOverHTTP below.
func TestE2ERouterReloadWithoutHoldersReturns404(t *testing.T) {
	pathA, pathB := writeTwoRouteCSVs(t)
	specs := routeFlagList{
		fmt.Sprintf("v1::rules:%s", pathA),
		fmt.Sprintf("v2::rules:%s", pathB),
	}
	routes, _, _, err := buildRoutes(specs, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	r := router.New(routes, router.DefaultPolicy{})
	srv := httptest.NewServer(wireRouterHandler(r, nil, nil, guardrailsWire{}, metricsWiring{}, nil, ""))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/admin/reload", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/admin/reload without holders: status = %d, want 404", resp.StatusCode)
	}
}

// TestE2ERouterPerRouteReloadOverHTTP is the load-bearing test for
// per-route reload: with holders mounted, POST /admin/reload
// {"model_version":"v1"} runs the v1 route's loader and swaps the v1
// holder's inner, while the v2 holder is untouched. The test
// overwrites the v1 CSV on disk with a new factor, reloads v1, then
// asserts /decide returns the new factor for v1 traffic AND the
// original factor for v2 traffic. The asymmetry between v1 (changed)
// and v2 (unchanged) is the proof -- if the reload accidentally
// swapped both holders, or neither, the test fails.
func TestE2ERouterPerRouteReloadOverHTTP(t *testing.T) {
	pathA, pathB := writeTwoRouteCSVs(t)
	specs := routeFlagList{
		fmt.Sprintf("v1:control:rules:%s", pathA),
		fmt.Sprintf("v2:treatment:rules:%s", pathB),
	}
	routes, holders, _, err := buildRoutes(specs, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	r := router.New(routes, router.DefaultPolicy{})
	srv := httptest.NewServer(wireRouterHandler(r, nil, holders, guardrailsWire{}, metricsWiring{}, nil, ""))
	t.Cleanup(srv.Close)

	// DefaultPolicy always picks the first route (v1); we need to
	// observe v2 traffic too. Probe with a few correlation IDs to
	// confirm DefaultPolicy ignores them (every probe must report v1).
	pre := decideRouterFactor(t, srv.URL, "x")
	if pre.modelVersion != "v1" || pre.factor != 1.10 {
		t.Fatalf("pre-reload v1 factor = %v / model = %q, want 1.10 / v1", pre.factor, pre.modelVersion)
	}

	// Overwrite the v1 CSV with a different factor and reload v1 only.
	const updatedV1 = `name,condition,factor,priority
enterprise,customer_tier == 'enterprise',1.99,10
`
	if err := os.WriteFile(pathA, []byte(updatedV1), 0o644); err != nil {
		t.Fatalf("overwrite v1 CSV: %v", err)
	}
	reloadResp, err := http.Post(srv.URL+"/admin/reload", "application/json",
		strings.NewReader(`{"model_version":"v1"}`))
	if err != nil {
		t.Fatalf("POST /admin/reload: %v", err)
	}
	if reloadResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(reloadResp.Body)
		reloadResp.Body.Close()
		t.Fatalf("/admin/reload status = %d, want 200; body=%s", reloadResp.StatusCode, body)
	}
	reloadResp.Body.Close()

	// Post-reload v1 must serve the new factor.
	post := decideRouterFactor(t, srv.URL, "x")
	if post.modelVersion != "v1" || post.factor != 1.99 {
		t.Fatalf("post-reload v1 factor = %v / model = %q, want 1.99 / v1", post.factor, post.modelVersion)
	}

	// v2 was not reloaded -- temporarily swap to HashCorrelationPolicy
	// to find a correlation ID that routes to v2, then assert the v2
	// factor is still its original 1.50 (not 1.99). To do this without
	// rebuilding the server we use the holders directly.
	v2 := findV2Holder(holders)
	if v2 == nil {
		t.Fatal("v2 holder missing in test setup")
	}
	v2Decision, err := v2.holder.Decide(context.Background(), markupReq())
	if err != nil {
		t.Fatalf("v2 Decide: %v", err)
	}
	if v2Decision.MarkupFactor != 1.50 {
		t.Errorf("v2 factor = %v, want 1.50 (v2 must not be swapped by v1 reload)", v2Decision.MarkupFactor)
	}
}

func TestE2ERouterReloadUnknownModelReturns404(t *testing.T) {
	pathA, pathB := writeTwoRouteCSVs(t)
	specs := routeFlagList{
		fmt.Sprintf("v1::rules:%s", pathA),
		fmt.Sprintf("v2::rules:%s", pathB),
	}
	routes, holders, _, err := buildRoutes(specs, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	r := router.New(routes, router.DefaultPolicy{})
	srv := httptest.NewServer(wireRouterHandler(r, nil, holders, guardrailsWire{}, metricsWiring{}, nil, ""))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/admin/reload", "application/json",
		strings.NewReader(`{"model_version":"v-does-not-exist"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2ERouterReloadMalformedBodyReturns400(t *testing.T) {
	pathA, pathB := writeTwoRouteCSVs(t)
	specs := routeFlagList{
		fmt.Sprintf("v1::rules:%s", pathA),
		fmt.Sprintf("v2::rules:%s", pathB),
	}
	routes, holders, _, err := buildRoutes(specs, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	r := router.New(routes, router.DefaultPolicy{})
	srv := httptest.NewServer(wireRouterHandler(r, nil, holders, guardrailsWire{}, metricsWiring{}, nil, ""))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/admin/reload", "application/json",
		strings.NewReader(`not json`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestE2ERouterReloadFailingLoaderReturns500AndKeepsOldDecider(t *testing.T) {
	pathA, pathB := writeTwoRouteCSVs(t)
	specs := routeFlagList{
		fmt.Sprintf("v1::rules:%s", pathA),
		fmt.Sprintf("v2::rules:%s", pathB),
	}
	routes, holders, _, err := buildRoutes(specs, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	r := router.New(routes, router.DefaultPolicy{})
	srv := httptest.NewServer(wireRouterHandler(r, nil, holders, guardrailsWire{}, metricsWiring{}, nil, ""))
	t.Cleanup(srv.Close)

	// Corrupt the v1 CSV so the loader fails on reload.
	if err := os.WriteFile(pathA, []byte(`"unterminated,1.0,0`), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/admin/reload", "application/json",
		strings.NewReader(`{"model_version":"v1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	// v1 holder must still serve the original factor (1.10).
	v1 := findHolder(holders, "v1")
	if v1 == nil {
		t.Fatal("v1 holder missing")
	}
	got, _ := v1.holder.Decide(context.Background(), markupReq())
	if got.MarkupFactor != 1.10 {
		t.Errorf("v1 factor after failed reload = %v, want 1.10", got.MarkupFactor)
	}
}

func TestE2ERouterReloadGetMethodReturns405(t *testing.T) {
	pathA, pathB := writeTwoRouteCSVs(t)
	specs := routeFlagList{
		fmt.Sprintf("v1::rules:%s", pathA),
		fmt.Sprintf("v2::rules:%s", pathB),
	}
	routes, holders, _, err := buildRoutes(specs, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	r := router.New(routes, router.DefaultPolicy{})
	srv := httptest.NewServer(wireRouterHandler(r, nil, holders, guardrailsWire{}, metricsWiring{}, nil, ""))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/admin/reload")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want POST", got)
	}
}

func findV2Holder(holders []routeHolder) *routeHolder {
	return findHolder(holders, "v2")
}

func findHolder(holders []routeHolder, modelVersion string) *routeHolder {
	for i := range holders {
		if holders[i].modelVersion == modelVersion {
			return &holders[i]
		}
	}
	return nil
}

func markupReq() markup.Request {
	return markup.Request{CustomerTier: "enterprise"}
}

func TestBuildRoutesRejectsMalformedSpec(t *testing.T) {
	specs := routeFlagList{"v1:variant"}
	_, _, _, err := buildRoutes(specs, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "want model:variant:type:path") {
		t.Fatalf("got %v, want format error", err)
	}
}

func TestBuildRoutesRejectsEmptyModel(t *testing.T) {
	specs := routeFlagList{":control:rules:r.csv"}
	_, _, _, err := buildRoutes(specs, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "model field is required") {
		t.Fatalf("got %v, want model-required error", err)
	}
}

func TestBuildRoutesRejectsBadSourceType(t *testing.T) {
	specs := routeFlagList{"v1:control:invalid:r.csv"}
	_, _, _, err := buildRoutes(specs, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "rules|snapshot") {
		t.Fatalf("got %v, want type error", err)
	}
}

func TestBuildRoutesPropagatesLoadError(t *testing.T) {
	specs := routeFlagList{"v1:control:rules:/path/does/not/exist.csv"}
	_, _, _, err := buildRoutes(specs, io.Discard)
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

// TestE2ERouterModeGuardrailsAdminMounts confirms the
// --guardrails-admin endpoint mounts in router mode and that a POST
// to it tightens the active envelope so a previously-passing route
// returns 500 afterward. The asymmetry across the same correlation
// ID before and after the admin POST is the proof.
func TestE2ERouterModeGuardrailsAdminMounts(t *testing.T) {
	pathA, pathB := writeTwoRouteCSVs(t)
	specs := routeFlagList{
		fmt.Sprintf("v1:control:rules:%s", pathA),
		fmt.Sprintf("v2:treatment:rules:%s", pathB),
	}
	routes, holders, _, err := buildRoutes(specs, io.Discard)
	if err != nil {
		t.Fatalf("buildRoutes: %v", err)
	}
	policy, _ := pickRouterPolicy("hash-correlation")
	r := router.New(routes, policy)

	// Admin enabled, no boot rules -- the Holder starts empty, both
	// routes pass everything until the admin POST tightens.
	gw := buildGuardrailsWiring(true, nil, io.Discard)
	srv := httptest.NewServer(wireRouterHandler(r, nil, holders, gw, metricsWiring{}, nil, ""))
	t.Cleanup(srv.Close)

	// Probe baseline: both routes produce factors (1.10 and 1.50 in
	// the two CSVs); without any active rule, both serve 200.
	idA, idB := findDifferingCorrelationIDs(t, srv.URL)
	if got := decideRouterFactor(t, srv.URL, idA); got.factor != 1.10 && got.factor != 1.50 {
		t.Fatalf("baseline idA factor = %v, want 1.10 or 1.50", got.factor)
	}

	// Tighten via admin: bound max=1.30 vetoes the 1.50 route but
	// allows the 1.10 route.
	adminResp, err := http.Post(srv.URL+"/admin/guardrails", "application/json",
		strings.NewReader(`{"factor_range":{"min":0.0,"max":1.30}}`))
	if err != nil {
		t.Fatalf("admin POST: %v", err)
	}
	adminResp.Body.Close()
	if adminResp.StatusCode != http.StatusOK {
		t.Fatalf("admin POST status = %d, want 200", adminResp.StatusCode)
	}

	// Post-admin: the 1.50 route now 500s; the 1.10 route still 200s.
	// We check both IDs and assert at least one flipped to 500.
	rawA := rawDecide(t, srv.URL, idA)
	rawB := rawDecide(t, srv.URL, idB)
	if rawA != 500 && rawB != 500 {
		t.Errorf("neither route vetoed after admin tightening; statuses = %d, %d", rawA, rawB)
	}
	if rawA == 500 && rawB == 500 {
		t.Errorf("both routes vetoed after max=1.30; one should still pass at 1.10")
	}
}

// rawDecide returns the bare /decide status code for a given
// correlation ID. Used when the test cares only about pass-vs-veto,
// not the Decision body.
func rawDecide(t *testing.T, baseURL, correlationID string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/decide",
		strings.NewReader(`{"customer_tier":"enterprise"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Correlation-ID", correlationID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
