package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/jsonlog"
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

// TestDecide_DefaultDecisionIDIs32CharHex pins the ADR-0035 default
// ID format. The handler's default newDecisionID() returns 16 bytes
// hex-encoded; observability tools rely on a stable parser.
func TestDecide_DefaultDecisionIDIs32CharHex(t *testing.T) {
	stub := &stubDecider{decision: markup.Decision{MarkupFactor: 1.0, Rule: "x", ModelVersion: "v1", EngineAdapter: "*x.Engine"}}
	attrs := captureDecisionEvent(t, httpapi.Decide(stub), `{"country":"DE"}`)
	got, _ := attrs["decision_id"].(string)
	if len(got) != 32 {
		t.Fatalf("decision_id length = %d, want 32 (16-byte hex); got %q", len(got), got)
	}
	for _, c := range got {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("decision_id %q contains non-hex %q", got, c)
		}
	}
}

// TestDecide_WithDecisionIDSourceInjectsValue confirms tests can pin
// a deterministic ID per the ADR-0035 option contract.
func TestDecide_WithDecisionIDSourceInjectsValue(t *testing.T) {
	stub := &stubDecider{decision: markup.Decision{MarkupFactor: 1.0, Rule: "x", ModelVersion: "v1", EngineAdapter: "*x.Engine"}}
	h := httpapi.Decide(stub, httpapi.WithDecisionIDSource(func() string { return "det-fixed-42" }))
	attrs := captureDecisionEvent(t, h, `{"country":"DE"}`)
	if attrs["decision_id"] != "det-fixed-42" {
		t.Fatalf("decision_id = %v, want det-fixed-42", attrs["decision_id"])
	}
}

// TestDecide_OutcomeMapsAcrossErrorPaths pins decide_outcome semantics
// per ADR-0035: ok | no_match | canceled | deadline_exceeded | error.
func TestDecide_OutcomeMapsAcrossErrorPaths(t *testing.T) {
	cases := []struct {
		name     string
		decision markup.Decision
		err      error
		wantOut  string
		wantHTTP int
		wantErr  bool
	}{
		{"ok", markup.Decision{MarkupFactor: 1.0, Rule: "x", ModelVersion: "v1", EngineAdapter: "*x.Engine"}, nil, "ok", http.StatusOK, false},
		{"no_match", markup.Decision{}, markup.ErrNoMatch, "no_match", http.StatusNotFound, false},
		{"canceled", markup.Decision{}, context.Canceled, "canceled", http.StatusInternalServerError, true},
		{"deadline", markup.Decision{}, context.DeadlineExceeded, "deadline_exceeded", http.StatusInternalServerError, true},
		{"error", markup.Decision{}, errors.New("boom"), "error", http.StatusInternalServerError, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubDecider{decision: tc.decision, err: tc.err}
			h := httpapi.Decide(stub, httpapi.WithDecisionIDSource(func() string { return "det-x" }))
			attrs, rec := captureDecisionEventAndStatus(t, h, `{"country":"DE"}`)
			if rec != tc.wantHTTP {
				t.Errorf("HTTP = %d, want %d", rec, tc.wantHTTP)
			}
			if attrs["decide_outcome"] != tc.wantOut {
				t.Errorf("decide_outcome = %v, want %v", attrs["decide_outcome"], tc.wantOut)
			}
			if tc.wantErr && attrs["error"] == "" {
				t.Errorf("error attr should be populated on %s; got empty", tc.name)
			}
			if !tc.wantErr && attrs["error"] != "" {
				t.Errorf("error attr should be empty on %s; got %v", tc.name, attrs["error"])
			}
		})
	}
}

// TestDecide_EmptyDecisionIDSuppressesEvent pins the silent-skip
// contract: if the IDSource returns "", the markup.decision.v1 event
// is not emitted but the HTTP response succeeds normally. Crypto/rand
// failure on a degraded host falls through this path.
func TestDecide_EmptyDecisionIDSuppressesEvent(t *testing.T) {
	stub := &stubDecider{decision: markup.Decision{MarkupFactor: 1.0, Rule: "x", ModelVersion: "v1", EngineAdapter: "*x.Engine"}}
	var buf bytes.Buffer
	l := jsonlog.New(&buf)
	h := httpapi.WithAccessLog(l, "test", nil, httpapi.Decide(stub, httpapi.WithDecisionIDSource(func() string { return "" })))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/decide", strings.NewReader(`{"country":"DE"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP = %d, want 200", rec.Code)
	}
	dec := json.NewDecoder(&buf)
	var events []map[string]any
	for dec.More() {
		var m map[string]any
		_ = dec.Decode(&m)
		events = append(events, m)
	}
	for _, e := range events {
		if e["msg"] == "markup.decision.v1" {
			t.Fatalf("markup.decision.v1 should not emit when decisionID is empty; got %v", e)
		}
	}
}

// captureDecisionEvent boots the Decide handler behind the access-log
// middleware, posts the body, and returns the markup.decision.v1 attrs.
func captureDecisionEvent(t *testing.T, decide http.Handler, body string) map[string]any {
	attrs, _ := captureDecisionEventAndStatus(t, decide, body)
	return attrs
}

func captureDecisionEventAndStatus(t *testing.T, decide http.Handler, body string) (map[string]any, int) {
	t.Helper()
	var buf bytes.Buffer
	l := jsonlog.New(&buf)
	h := httpapi.WithAccessLog(l, "test", nil, decide)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/decide", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	dec := json.NewDecoder(&buf)
	var events []map[string]any
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		events = append(events, m)
	}
	for _, e := range events {
		if e["msg"] == "markup.decision.v1" {
			return e["attrs"].(map[string]any), rec.Code
		}
	}
	t.Fatalf("markup.decision.v1 event not emitted; events=%v", events)
	return nil, 0
}
