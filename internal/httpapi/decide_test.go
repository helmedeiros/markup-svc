package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

type stubDecider struct {
	decision markup.Decision
	err      error
	gotReq   markup.Request
	gotCtx   context.Context
}

func (s *stubDecider) Decide(ctx context.Context, req markup.Request) (markup.Decision, error) {
	s.gotReq = req
	s.gotCtx = ctx
	return s.decision, s.err
}

func post(handler http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/decide", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestDecideHappyPathReturns200WithDecisionJSON(t *testing.T) {
	stub := &stubDecider{
		decision: markup.Decision{
			MarkupFactor:  1.15,
			Rule:          "enterprise",
			ModelVersion:  "v1",
			CorrelationID: "abc",
			EngineAdapter: "*inmemory.Engine",
		},
	}
	h := httpapi.Decide(stub)
	rec := post(h, `{"customer_tier":"enterprise","country":"DE"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}
	if body["markup_factor"].(float64) != 1.15 {
		t.Errorf("markup_factor = %v, want 1.15", body["markup_factor"])
	}
	if body["rule"] != "enterprise" {
		t.Errorf("rule = %v, want enterprise", body["rule"])
	}
	if body["engine_adapter"] != "*inmemory.Engine" {
		t.Errorf("engine_adapter = %v", body["engine_adapter"])
	}
	if _, present := body["experiment"]; present {
		t.Errorf("experiment must be omitted when empty; got body = %v", body)
	}
	if stub.gotReq.CustomerTier != "enterprise" {
		t.Errorf("decider received CustomerTier = %q", stub.gotReq.CustomerTier)
	}
	if stub.gotReq.Country != "DE" {
		t.Errorf("decider received Country = %q", stub.gotReq.Country)
	}
}

func TestDecideErrNoMatchReturns404(t *testing.T) {
	stub := &stubDecider{err: markup.ErrNoMatch}
	rec := post(httpapi.Decide(stub), `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["error"] != "no rule matched" {
		t.Errorf("error body = %q", body["error"])
	}
}

func TestDecideOtherErrorReturns500WithOpaqueBody(t *testing.T) {
	stub := &stubDecider{err: errors.New("some internal failure with private detail")}
	rec := post(httpapi.Decide(stub), `{}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "private detail") {
		t.Errorf("500 body must not leak underlying error: %s", rec.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "internal" {
		t.Errorf("error body = %q, want %q", body["error"], "internal")
	}
}

func TestDecideMalformedJSONReturns400(t *testing.T) {
	stub := &stubDecider{}
	rec := post(httpapi.Decide(stub), `not json at all`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if stub.gotCtx != nil {
		t.Errorf("Decider must not be called on malformed input; got call with ctx %v", stub.gotCtx)
	}
}

func TestDecideRejectsNonPOSTWith405AndAllowHeader(t *testing.T) {
	stub := &stubDecider{}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/decide", nil)
		rec := httptest.NewRecorder()
		httpapi.Decide(stub).ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodPost {
			t.Errorf("%s: Allow header = %q, want POST", method, got)
		}
	}
	if stub.gotCtx != nil {
		t.Errorf("Decider must not be called on non-POST methods")
	}
}

func TestDecideEmptyBodyReturns400(t *testing.T) {
	stub := &stubDecider{}
	rec := post(httpapi.Decide(stub), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
