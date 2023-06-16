package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/swap"
	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

type spyBodyLoader struct {
	supports map[string]bool
	calls    atomic.Int32
	lastCT   string
	lastBody []byte
	decider  markup.Decider
	result   httpapi.ReloadResult
	err      error
}

func (s *spyBodyLoader) Supports(ct string) bool { return s.supports[ct] }
func (s *spyBodyLoader) Load(ct string, body []byte) (markup.Decider, httpapi.ReloadResult, error) {
	s.calls.Add(1)
	s.lastCT = ct
	s.lastBody = body
	return s.decider, s.result, s.err
}

func newSpyLoader() *spyBodyLoader {
	return &spyBodyLoader{supports: map[string]bool{"text/csv": true, "application/json": true}}
}

type spyFileLoader struct {
	calls   atomic.Int32
	decider markup.Decider
	result  httpapi.ReloadResult
	err     error
}

func (s *spyFileLoader) loader() httpapi.Loader {
	return func() (markup.Decider, httpapi.ReloadResult, error) {
		s.calls.Add(1)
		return s.decider, s.result, s.err
	}
}

func TestReload_EmptyBody_FileBasedPath_Unchanged(t *testing.T) {
	initial := &stubDecider{decision: markup.Decision{Rule: "initial"}}
	holder := swap.New(initial)
	next := &stubDecider{decision: markup.Decision{Rule: "next"}}
	fileSpy := &spyFileLoader{decider: next, result: httpapi.ReloadResult{RuleCount: 7, ModelVersion: "v3"}}
	body := newSpyLoader()

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	rec := httptest.NewRecorder()
	httpapi.Reload(holder, fileSpy.loader(), httpapi.WithReloadBodyLoader(body)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body.calls.Load() != 0 {
		t.Errorf("body-loader called %d times on empty-body POST, want 0", body.calls.Load())
	}
	if fileSpy.calls.Load() != 1 {
		t.Errorf("file-loader called %d times, want 1", fileSpy.calls.Load())
	}
}

func TestReload_BodyLoaderWired_EmptyBody_StillHitsFilePath(t *testing.T) {
	holder := swap.New(&stubDecider{decision: markup.Decision{Rule: "initial"}})
	fileSpy := &spyFileLoader{decider: &stubDecider{}, result: httpapi.ReloadResult{RuleCount: 1, ModelVersion: "v1"}}
	body := newSpyLoader()
	body.supports = map[string]bool{} // even if loader supports nothing, file path still works

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	rec := httptest.NewRecorder()
	httpapi.Reload(holder, fileSpy.loader(), httpapi.WithReloadBodyLoader(body)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body.calls.Load() != 0 {
		t.Errorf("body-loader called on empty-body POST, want 0")
	}
	if fileSpy.calls.Load() != 1 {
		t.Errorf("file-loader call count = %d, want 1", fileSpy.calls.Load())
	}
}

func TestReload_EmptyBody_RecognizedContentType_StillHitsFilePath(t *testing.T) {
	holder := swap.New(&stubDecider{decision: markup.Decision{Rule: "initial"}})
	fileSpy := &spyFileLoader{decider: &stubDecider{}, result: httpapi.ReloadResult{}}
	body := newSpyLoader()

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	req.Header.Set("Content-Type", "text/csv")
	rec := httptest.NewRecorder()
	httpapi.Reload(holder, fileSpy.loader(), httpapi.WithReloadBodyLoader(body)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body.calls.Load() != 0 {
		t.Errorf("body-loader called on empty-body POST with text/csv, want 0")
	}
	if fileSpy.calls.Load() != 1 {
		t.Errorf("file-loader call count = %d, want 1", fileSpy.calls.Load())
	}
}

func TestReload_TextCSV_HappyPath(t *testing.T) {
	initial := &stubDecider{decision: markup.Decision{Rule: "initial"}}
	holder := swap.New(initial)
	next := &stubDecider{decision: markup.Decision{Rule: "next"}}
	fileSpy := &spyFileLoader{}
	body := newSpyLoader()
	body.decider = next
	body.result = httpapi.ReloadResult{RuleCount: 5, ModelVersion: "v9"}

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", strings.NewReader("name,condition,factor,priority\n"))
	req.Header.Set("Content-Type", "text/csv")
	rec := httptest.NewRecorder()
	httpapi.Reload(holder, fileSpy.loader(), httpapi.WithReloadBodyLoader(body)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if v, _ := resp["rule_count"].(float64); int(v) != 5 {
		t.Errorf("rule_count = %v, want 5", resp["rule_count"])
	}
	if v, _ := resp["model_version"].(string); v != "v9" {
		t.Errorf("model_version = %v, want v9", v)
	}
	if body.calls.Load() != 1 {
		t.Errorf("body-loader call count = %d, want 1", body.calls.Load())
	}
	if fileSpy.calls.Load() != 0 {
		t.Errorf("file-loader called on body-based path, want 0")
	}
	got, _ := holder.Decide(context.Background(), markup.Request{})
	if got.Rule != "next" {
		t.Errorf("holder Decider after swap = %q, want next", got.Rule)
	}
}

func TestReload_TextCSV_WithCharset(t *testing.T) {
	holder := swap.New(&stubDecider{decision: markup.Decision{Rule: "initial"}})
	body := newSpyLoader()
	body.decider = &stubDecider{decision: markup.Decision{Rule: "next"}}
	body.result = httpapi.ReloadResult{RuleCount: 1, ModelVersion: "v1"}

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", strings.NewReader("name,condition,factor,priority\n"))
	req.Header.Set("Content-Type", "text/csv; charset=utf-8")
	rec := httptest.NewRecorder()
	httpapi.Reload(holder, (&spyFileLoader{}).loader(), httpapi.WithReloadBodyLoader(body)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if body.lastCT != "text/csv" {
		t.Errorf("body-loader received media type %q, want normalized text/csv", body.lastCT)
	}
}

func TestReload_TextCSV_InvalidRuleSet(t *testing.T) {
	holder := swap.New(&stubDecider{decision: markup.Decision{Rule: "initial"}})
	body := newSpyLoader()
	body.err = &markup.InvalidRuleSetError{Path: "<body>", Err: errors.New("parse: bad row")}

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", strings.NewReader("garbage"))
	req.Header.Set("Content-Type", "text/csv")
	rec := httptest.NewRecorder()
	httpapi.Reload(holder, (&spyFileLoader{}).loader(), httpapi.WithReloadBodyLoader(body)).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestReload_TextCSV_DiagnoseRejected(t *testing.T) {
	holder := swap.New(&stubDecider{decision: markup.Decision{Rule: "initial"}})
	diag := markup.Diagnosis{Issues: []markup.Issue{{Kind: "duplicate_name", Severity: markup.SeverityError, Rule: "dup", Detail: "twice"}}}
	body := newSpyLoader()
	body.err = &httpapi.DiagnoseRejectedError{Diagnosis: diag}

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", strings.NewReader("dup,..."))
	req.Header.Set("Content-Type", "text/csv")
	rec := httptest.NewRecorder()
	httpapi.Reload(holder, (&spyFileLoader{}).loader(), httpapi.WithReloadBodyLoader(body)).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if v, _ := resp["healthy"].(bool); v {
		t.Errorf("healthy = true, want false")
	}
	errs, _ := resp["errors"].([]any)
	if len(errs) != 1 {
		t.Errorf("errors len = %d, want 1; body=%s", len(errs), rec.Body.String())
	}
}

func TestReload_ApplicationJSON_HappyPath(t *testing.T) {
	holder := swap.New(&stubDecider{decision: markup.Decision{Rule: "initial"}})
	body := newSpyLoader()
	body.decider = &stubDecider{decision: markup.Decision{Rule: "snap-next"}}
	body.result = httpapi.ReloadResult{RuleCount: 42, ModelVersion: "snap-v3"}

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", strings.NewReader(`{"snapshot":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	httpapi.Reload(holder, (&spyFileLoader{}).loader(), httpapi.WithReloadBodyLoader(body)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if body.lastCT != "application/json" {
		t.Errorf("media type = %q, want application/json", body.lastCT)
	}
}

func TestReload_UnrecognizedContentType_FallsThrough(t *testing.T) {
	holder := swap.New(&stubDecider{decision: markup.Decision{Rule: "initial"}})
	fileSpy := &spyFileLoader{decider: &stubDecider{}, result: httpapi.ReloadResult{}}
	body := newSpyLoader()

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", strings.NewReader("some=data"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	httpapi.Reload(holder, fileSpy.loader(), httpapi.WithReloadBodyLoader(body)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body.calls.Load() != 0 {
		t.Errorf("body-loader called for unrecognized type, want 0")
	}
	if fileSpy.calls.Load() != 1 {
		t.Errorf("file-loader call count = %d, want 1", fileSpy.calls.Load())
	}
}

func TestReload_NoBodyLoader_FallsThrough(t *testing.T) {
	holder := swap.New(&stubDecider{decision: markup.Decision{Rule: "initial"}})
	fileSpy := &spyFileLoader{decider: &stubDecider{}, result: httpapi.ReloadResult{}}

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", strings.NewReader("name,condition,factor,priority\n"))
	req.Header.Set("Content-Type", "text/csv")
	rec := httptest.NewRecorder()
	httpapi.Reload(holder, fileSpy.loader()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if fileSpy.calls.Load() != 1 {
		t.Errorf("file-loader call count = %d, want 1", fileSpy.calls.Load())
	}
}

func TestReload_NoContentType_FallsThrough(t *testing.T) {
	holder := swap.New(&stubDecider{decision: markup.Decision{Rule: "initial"}})
	fileSpy := &spyFileLoader{decider: &stubDecider{}, result: httpapi.ReloadResult{}}
	body := newSpyLoader()

	req := httptest.NewRequest(http.MethodPost, "/admin/reload", strings.NewReader("foo"))
	rec := httptest.NewRecorder()
	httpapi.Reload(holder, fileSpy.loader(), httpapi.WithReloadBodyLoader(body)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body.calls.Load() != 0 {
		t.Errorf("body-loader called with no Content-Type, want 0")
	}
	if fileSpy.calls.Load() != 1 {
		t.Errorf("file-loader call count = %d, want 1", fileSpy.calls.Load())
	}
}

func TestReload_BodyExceedsCap_Returns413(t *testing.T) {
	holder := swap.New(&stubDecider{decision: markup.Decision{Rule: "initial"}})
	body := newSpyLoader()

	big := bytes.Repeat([]byte("a"), 17*1024*1024)
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", bytes.NewReader(big))
	req.Header.Set("Content-Type", "text/csv")
	rec := httptest.NewRecorder()
	httpapi.Reload(holder, (&spyFileLoader{}).loader(), httpapi.WithReloadBodyLoader(body)).ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
}
