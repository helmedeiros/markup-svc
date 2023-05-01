package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

func TestDiagnose_HealthyReturns200(t *testing.T) {
	fn := func() (markup.Diagnosis, error) { return markup.Diagnosis{}, nil }
	rec := httptest.NewRecorder()
	httpapi.Diagnose(fn).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/diagnose", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["healthy"] != true {
		t.Errorf("healthy = %v, want true", got["healthy"])
	}
}

func TestDiagnose_UnhealthyReturns503(t *testing.T) {
	fn := func() (markup.Diagnosis, error) {
		return markup.Diagnosis{Issues: []markup.Issue{
			{Kind: markup.IssueEmptyRuleSet, Severity: markup.SeverityError, Detail: "empty"},
		}}, nil
	}
	rec := httptest.NewRecorder()
	httpapi.Diagnose(fn).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/diagnose", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["healthy"] != false {
		t.Errorf("healthy = %v, want false", got["healthy"])
	}
	if len(got["errors"].([]any)) != 1 {
		t.Errorf("errors = %v", got["errors"])
	}
}

func TestDiagnose_NonGetReturns405(t *testing.T) {
	fn := func() (markup.Diagnosis, error) { return markup.Diagnosis{}, nil }
	rec := httptest.NewRecorder()
	httpapi.Diagnose(fn).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/diagnose", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
