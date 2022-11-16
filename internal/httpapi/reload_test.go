package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/swap"
	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

func loaderReturning(decider markup.Decider, result httpapi.ReloadResult) httpapi.Loader {
	return func() (markup.Decider, httpapi.ReloadResult, error) {
		return decider, result, nil
	}
}

func loaderFailing(err error) httpapi.Loader {
	return func() (markup.Decider, httpapi.ReloadResult, error) {
		return nil, httpapi.ReloadResult{}, err
	}
}

func TestReloadHappyPath200WithJSONBody(t *testing.T) {
	initial := &stubDecider{decision: markup.Decision{Rule: "initial"}}
	holder := swap.New(initial)

	next := &stubDecider{decision: markup.Decision{Rule: "next"}}
	loader := loaderReturning(next, httpapi.ReloadResult{RuleCount: 42, ModelVersion: "v2"})

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	rec := httptest.NewRecorder()
	httpapi.Reload(holder, loader).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["rule_count"].(float64) != 42 {
		t.Errorf("rule_count = %v, want 42", body["rule_count"])
	}
	if body["model_version"] != "v2" {
		t.Errorf("model_version = %v, want \"v2\"", body["model_version"])
	}
}

func TestReloadSwapsHolderSoNextDecideUsesNewDecider(t *testing.T) {
	initial := &stubDecider{decision: markup.Decision{Rule: "initial"}}
	holder := swap.New(initial)

	next := &stubDecider{decision: markup.Decision{Rule: "next"}}
	loader := loaderReturning(next, httpapi.ReloadResult{RuleCount: 1, ModelVersion: "v2"})

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	rec := httptest.NewRecorder()
	httpapi.Reload(holder, loader).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reload status = %d", rec.Code)
	}

	got, err := holder.Decide(context.Background(), markup.Request{})
	if err != nil {
		t.Fatalf("post-reload Decide: %v", err)
	}
	if got.Rule != "next" {
		t.Fatalf("post-reload Rule = %q, want \"next\" (holder must hold next after Swap)", got.Rule)
	}
}

func TestReloadLoaderErrorReturns500WithOpaqueBody(t *testing.T) {
	holder := swap.New(&stubDecider{decision: markup.Decision{Rule: "initial"}})
	loader := loaderFailing(errors.New("disk read failed -- some internal detail"))

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	rec := httptest.NewRecorder()
	httpapi.Reload(holder, loader).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "internal detail") {
		t.Errorf("500 body must not leak underlying error: %s", rec.Body.String())
	}
	got, _ := holder.Decide(context.Background(), markup.Request{})
	if got.Rule != "initial" {
		t.Errorf("after failed reload holder Rule = %q, want \"initial\" (no swap on loader failure)", got.Rule)
	}
}

func TestReloadRejectsNonPOSTWith405AndAllowHeader(t *testing.T) {
	holder := swap.New(&stubDecider{decision: markup.Decision{Rule: "initial"}})
	loaderCalled := false
	loader := httpapi.Loader(func() (markup.Decider, httpapi.ReloadResult, error) {
		loaderCalled = true
		return nil, httpapi.ReloadResult{}, nil
	})

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/admin/reload", nil)
		rec := httptest.NewRecorder()
		httpapi.Reload(holder, loader).ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodPost {
			t.Errorf("%s: Allow header = %q, want POST", method, got)
		}
	}
	if loaderCalled {
		t.Error("loader must not be invoked on non-POST methods")
	}
}
