package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/swap"
	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

func TestReload_WithDiagnoseGate_Rejects400OnUnhealthy(t *testing.T) {
	initial := &stubDecider{decision: markup.Decision{Rule: "initial"}}
	holder := swap.New(initial)
	next := &stubDecider{decision: markup.Decision{Rule: "next"}}
	loaderCalls := 0
	loader := func() (markup.Decider, httpapi.ReloadResult, error) {
		loaderCalls++
		return next, httpapi.ReloadResult{RuleCount: 5, ModelVersion: "v1"}, nil
	}
	gate := func() (markup.Diagnosis, error) {
		return markup.Diagnosis{Issues: []markup.Issue{
			{Kind: markup.IssueEmptyRuleSet, Severity: markup.SeverityError, Detail: "rule set is empty"},
		}}, nil
	}

	rec := httptest.NewRecorder()
	httpapi.Reload(holder, loader, httpapi.WithReloadDiagnose(gate)).
		ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/reload", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 ; body=%s", rec.Code, rec.Body.String())
	}
	if loaderCalls != 0 {
		t.Errorf("loader called %d times; should not be called when diagnose fails", loaderCalls)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["healthy"] != false {
		t.Errorf("healthy = %v, want false", body["healthy"])
	}
	if errs, _ := body["errors"].([]any); len(errs) != 1 {
		t.Errorf("errors = %v", body["errors"])
	}
}

func TestReload_WithDiagnoseGate_AllowsHealthy(t *testing.T) {
	initial := &stubDecider{decision: markup.Decision{Rule: "initial"}}
	holder := swap.New(initial)
	next := &stubDecider{decision: markup.Decision{Rule: "next"}}
	loader := func() (markup.Decider, httpapi.ReloadResult, error) {
		return next, httpapi.ReloadResult{RuleCount: 5, ModelVersion: "v1"}, nil
	}
	gate := func() (markup.Diagnosis, error) {
		return markup.Diagnosis{Issues: []markup.Issue{
			{Kind: markup.IssueNoOpFactor, Severity: markup.SeverityWarning, Rule: "x", Detail: "factor 1.0"},
		}}, nil
	}

	rec := httptest.NewRecorder()
	httpapi.Reload(holder, loader, httpapi.WithReloadDiagnose(gate)).
		ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/reload", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 ; body=%s", rec.Code, rec.Body.String())
	}
}
