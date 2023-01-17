package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/httpapi"
)

func TestHealthzReturns200OnGet(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	httpapi.Healthz().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %q, want \"ok\"", body["status"])
	}
}

func TestHealthzRejectsNonGetWith405AndAllowHeader(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/healthz", nil)
		rec := httptest.NewRecorder()
		httpapi.Healthz().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodGet {
			t.Errorf("%s: Allow = %q, want GET", method, got)
		}
	}
}

func TestReadyzReturns200WhenReady(t *testing.T) {
	ready := httpapi.Ready(func() (string, bool) { return "", true })
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	httpapi.Readyz(ready).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "ready" {
		t.Errorf("status = %q, want \"ready\"", body["status"])
	}
}

func TestReadyzReturns503WithReasonWhenNotReady(t *testing.T) {
	ready := httpapi.Ready(func() (string, bool) { return "decider not built", false })
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	httpapi.Readyz(ready).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "not_ready" {
		t.Errorf("status = %q, want \"not_ready\"", body["status"])
	}
	if body["reason"] != "decider not built" {
		t.Errorf("reason = %q, want \"decider not built\"", body["reason"])
	}
}

func TestReadyzRejectsNonGetWith405AndAllowHeader(t *testing.T) {
	ready := httpapi.Ready(func() (string, bool) { return "", true })
	called := false
	gatedReady := httpapi.Ready(func() (string, bool) {
		called = true
		return "", true
	})
	_ = gatedReady // see negative-assert below
	_ = ready

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/readyz", nil)
		rec := httptest.NewRecorder()
		httpapi.Readyz(gatedReady).ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodGet {
			t.Errorf("%s: Allow = %q, want GET", method, got)
		}
	}
	if called {
		t.Errorf("ready closure must NOT be invoked on non-GET methods (called=%v)", called)
	}
}

func TestReadyzCallsClosureOnEveryProbe(t *testing.T) {
	calls := 0
	ready := httpapi.Ready(func() (string, bool) {
		calls++
		return "", true
	})
	handler := httpapi.Readyz(ready)

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
	if calls != 5 {
		t.Errorf("ready closure called %d times, want 5 (one per probe)", calls)
	}
}

func TestReadyzBodyIsValidJSON(t *testing.T) {
	cases := []struct {
		name  string
		ready httpapi.Ready
	}{
		{"ready", func() (string, bool) { return "", true }},
		{"not_ready", func() (string, bool) { return "decider not built", false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			rec := httptest.NewRecorder()
			httpapi.Readyz(tc.ready).ServeHTTP(rec, req)
			var body map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Errorf("body is not valid JSON: %v; body=%s", err, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
		})
	}
}
