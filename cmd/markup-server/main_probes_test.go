package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/router"
)

// TestE2EHealthzReturns200OnGet confirms the liveness probe is wired
// onto the same mux as /decide and returns the documented body.
func TestE2EHealthzReturns200OnGet(t *testing.T) {
	loader := rulesLoader("testdata/rules.csv", "inmemory", "v0-probe", io.Discard)
	handler, _, err := wireHandler(loader)
	if err != nil {
		t.Fatalf("wireHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("status = %q, want \"ok\"", body["status"])
	}
}

// TestE2EReadyzReturns200AfterBoot pins the cmd-side readiness
// contract: after wireHandler returns successfully, /readyz reports
// the service as ready. The kubelet only sends /decide traffic once
// /readyz returns 200, so a failure here would silently make a
// production deployment never receive traffic.
func TestE2EReadyzReturns200AfterBoot(t *testing.T) {
	loader := rulesLoader("testdata/rules.csv", "inmemory", "v0-probe", io.Discard)
	handler, _, err := wireHandler(loader)
	if err != nil {
		t.Fatalf("wireHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ready" {
		t.Errorf("status = %q, want \"ready\"", body["status"])
	}
}

func TestE2EProbesRejectNonGetWith405(t *testing.T) {
	loader := rulesLoader("testdata/rules.csv", "inmemory", "v0-probe", io.Discard)
	handler, _, err := wireHandler(loader)
	if err != nil {
		t.Fatalf("wireHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", path, resp.StatusCode)
		}
		if got := resp.Header.Get("Allow"); got != http.MethodGet {
			t.Errorf("%s: Allow = %q, want GET", path, got)
		}
	}
}

func TestE2EProbesMountedInRouterModeToo(t *testing.T) {
	pathA, pathB := writeTwoRouteCSVs(t)
	specs := routeFlagList{
		"v1:control:rules:" + pathA,
		"v2:treatment:rules:" + pathB,
	}
	routes, holders, _, err := buildRoutes(specs, io.Discard)
	if err != nil {
		t.Fatalf("buildRoutes: %v", err)
	}
	policy, _ := pickRouterPolicy("hash-correlation")
	srv := httptest.NewServer(wireRouterHandler(router.New(routes, policy), nil, holders, guardrailsWire{}, metricsWiring{}, nil, ""))
	t.Cleanup(srv.Close)

	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s in router mode: status = %d, want 200", path, resp.StatusCode)
		}
	}
}
