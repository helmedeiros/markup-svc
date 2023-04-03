package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/jsonlog"
)

func TestWithAccessLog_EmitsExpectedAttrs(t *testing.T) {
	var buf bytes.Buffer
	l := jsonlog.New(&buf)
	h := httpapi.WithAccessLog(l, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/decide", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("status=%d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v ; raw=%q", err, buf.String())
	}
	if got["msg"] != "markup-server.access" {
		t.Errorf("msg=%v", got["msg"])
	}
	attrs := got["attrs"].(map[string]any)
	if attrs["method"] != "POST" || attrs["path"] != "/decide" || attrs["status"].(float64) != 418 {
		t.Errorf("attrs=%v", attrs)
	}
	if _, ok := attrs["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms missing/not float: %v", attrs["duration_ms"])
	}
}

func TestWithAccessLog_NilLoggerIsPassThrough(t *testing.T) {
	called := false
	h := httpapi.WithAccessLog(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if !called {
		t.Errorf("inner not invoked")
	}
}
