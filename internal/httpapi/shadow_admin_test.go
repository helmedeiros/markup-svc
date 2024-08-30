package httpapi_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/shadow"
	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

type stubBodyLoader struct {
	supports func(string) bool
	load     func(string, []byte) (markup.Decider, httpapi.ReloadResult, error)
}

func (s stubBodyLoader) Supports(mediaType string) bool { return s.supports(mediaType) }
func (s stubBodyLoader) Load(mt string, body []byte) (markup.Decider, httpapi.ReloadResult, error) {
	return s.load(mt, body)
}

type stubChallengerDecider struct{ rule string }

func (s stubChallengerDecider) Decide(_ context.Context, _ markup.Request) (markup.Decision, error) {
	return markup.Decision{Rule: s.rule}, nil
}

func TestLoadChallengerInstallsDeciderOnSuccess(t *testing.T) {
	holder := shadow.New()
	body := stubBodyLoader{
		supports: func(mt string) bool { return mt == "text/csv" },
		load: func(_ string, _ []byte) (markup.Decider, httpapi.ReloadResult, error) {
			return stubChallengerDecider{rule: "challenger_v1"}, httpapi.ReloadResult{RuleCount: 3, ModelVersion: "v1"}, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/load-challenger", bytes.NewReader([]byte("alpha,a,1.0,1\n")))
	req.Header.Set("Content-Type", "text/csv")
	rec := httptest.NewRecorder()
	httpapi.LoadChallenger(holder, body).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	d, loaded := holder.Get()
	if !loaded {
		t.Fatal("holder did not install challenger")
	}
	got, _ := d.Decide(context.Background(), markup.Request{})
	if got.Rule != "challenger_v1" {
		t.Fatalf("installed wrong decider; rule=%q", got.Rule)
	}
	if !strings.Contains(rec.Body.String(), `"rule_count":3`) {
		t.Fatalf("response body missing rule_count: %s", rec.Body.String())
	}
}

func TestLoadChallengerSurfacesDiagnoseRejection(t *testing.T) {
	holder := shadow.New()
	body := stubBodyLoader{
		supports: func(string) bool { return true },
		load: func(string, []byte) (markup.Decider, httpapi.ReloadResult, error) {
			return nil, httpapi.ReloadResult{}, &httpapi.DiagnoseRejectedError{Diagnosis: markup.Diagnosis{
				Issues: []markup.Issue{{Kind: markup.IssueInvalidFactor, Severity: markup.SeverityError, Rule: "r1", Detail: "factor is negative"}},
			}}
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/load-challenger", bytes.NewReader([]byte("x")))
	req.Header.Set("Content-Type", "text/csv")
	rec := httptest.NewRecorder()
	httpapi.LoadChallenger(holder, body).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 on Diagnose failure", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"healthy":false`) {
		t.Fatalf("body missing healthy:false: %s", rec.Body.String())
	}
	if _, loaded := holder.Get(); loaded {
		t.Fatal("Diagnose failure installed a challenger anyway")
	}
}

func TestLoadChallengerRejectsEmptyBody(t *testing.T) {
	body := stubBodyLoader{supports: func(string) bool { return true }, load: nil}
	req := httptest.NewRequest(http.MethodPost, "/admin/load-challenger", bytes.NewReader(nil))
	req.Header.Set("Content-Type", "text/csv")
	rec := httptest.NewRecorder()
	httpapi.LoadChallenger(shadow.New(), body).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 on empty body", rec.Code)
	}
}

func TestLoadChallengerRejectsUnsupportedMediaType(t *testing.T) {
	body := stubBodyLoader{supports: func(string) bool { return false }}
	req := httptest.NewRequest(http.MethodPost, "/admin/load-challenger", bytes.NewReader([]byte("x")))
	req.Header.Set("Content-Type", "text/csv")
	rec := httptest.NewRecorder()
	httpapi.LoadChallenger(shadow.New(), body).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status=%d want 415", rec.Code)
	}
}

func TestLoadChallengerRejectsNonPOST(t *testing.T) {
	body := stubBodyLoader{supports: func(string) bool { return true }}
	req := httptest.NewRequest(http.MethodGet, "/admin/load-challenger", nil)
	rec := httptest.NewRecorder()
	httpapi.LoadChallenger(shadow.New(), body).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("Allow=%q want POST", rec.Header().Get("Allow"))
	}
}

func TestClearChallengerRemovesChallenger(t *testing.T) {
	holder := shadow.New()
	holder.Load(stubChallengerDecider{rule: "doomed"})

	req := httptest.NewRequest(http.MethodDelete, "/admin/challenger", nil)
	rec := httptest.NewRecorder()
	httpapi.ClearChallenger(holder).ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d want 204", rec.Code)
	}
	if _, loaded := holder.Get(); loaded {
		t.Fatal("ClearChallenger did not remove challenger")
	}
}

func TestClearChallengerIdempotentOnEmptyHolder(t *testing.T) {
	holder := shadow.New()
	req := httptest.NewRequest(http.MethodDelete, "/admin/challenger", nil)
	rec := httptest.NewRecorder()
	httpapi.ClearChallenger(holder).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d want 204 on idempotent clear", rec.Code)
	}
}

func TestLoadChallengerRejectsOversizeBody(t *testing.T) {
	body := stubBodyLoader{supports: func(string) bool { return true }}
	oversized := bytes.Repeat([]byte{'x'}, 17*1024*1024)
	req := httptest.NewRequest(http.MethodPost, "/admin/load-challenger", bytes.NewReader(oversized))
	req.Header.Set("Content-Type", "text/csv")
	rec := httptest.NewRecorder()
	httpapi.LoadChallenger(shadow.New(), body).ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want 413 on oversize body", rec.Code)
	}
}

func TestClearChallengerRejectsNonDELETE(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/admin/challenger", nil)
	rec := httptest.NewRecorder()
	httpapi.ClearChallenger(shadow.New()).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != http.MethodDelete {
		t.Fatalf("Allow=%q want DELETE", rec.Header().Get("Allow"))
	}
}
