package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	breengine "github.com/helmedeiros/bre-go/engine"
)

func TestWithCorrelationIDPassesThroughSuppliedHeader(t *testing.T) {
	var seenCtx context.Context
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenCtx = r.Context()
		w.WriteHeader(http.StatusOK)
	})
	mw := WithCorrelationID(next)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(CorrelationIDHeader, "supplied-id-abc")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if got := rec.Header().Get(CorrelationIDHeader); got != "supplied-id-abc" {
		t.Errorf("response header = %q, want \"supplied-id-abc\"", got)
	}
	if got := breengine.CorrelationIDFromContext(seenCtx); got != "supplied-id-abc" {
		t.Errorf("ctx ID = %q, want \"supplied-id-abc\"", got)
	}
}

func TestWithCorrelationIDGeneratesWhenHeaderAbsent(t *testing.T) {
	var seenCtx context.Context
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenCtx = r.Context()
		w.WriteHeader(http.StatusOK)
	})
	mw := WithCorrelationID(next)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	got := rec.Header().Get(CorrelationIDHeader)
	if got == "" {
		t.Fatal("response header empty; want generated UUID")
	}
	uuidV4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidV4.MatchString(got) {
		t.Errorf("response header %q is not a UUID v4", got)
	}
	if ctxID := breengine.CorrelationIDFromContext(seenCtx); ctxID != got {
		t.Errorf("ctx ID = %q, response header = %q; should match", ctxID, got)
	}
}

func TestWithCorrelationIDTreatsEmptyHeaderAsAbsent(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw := WithCorrelationID(next)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(CorrelationIDHeader, "")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	got := rec.Header().Get(CorrelationIDHeader)
	if got == "" {
		t.Fatal("empty supplied header should trigger generation")
	}
	uuidV4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidV4.MatchString(got) {
		t.Errorf("generated ID %q is not a UUID v4", got)
	}
}

func TestWithCorrelationIDReturns500WhenRandFails(t *testing.T) {
	orig := randRead
	defer func() { randRead = orig }()
	randRead = func(b []byte) (int, error) {
		return 0, errors.New("rand source unavailable")
	}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	mw := WithCorrelationID(next)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if called {
		t.Error("next handler must not run when correlation ID generation fails")
	}
}

func TestGenerateUUIDProducesV4Format(t *testing.T) {
	id, err := generateUUID()
	if err != nil {
		t.Fatalf("generateUUID: %v", err)
	}
	uuidV4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidV4.MatchString(id) {
		t.Errorf("generateUUID = %q, not a UUID v4", id)
	}
}

func TestGenerateUUIDIsUnique(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		id, err := generateUUID()
		if err != nil {
			t.Fatalf("generateUUID #%d: %v", i, err)
		}
		if seen[id] {
			t.Fatalf("collision: %q seen twice in 100 draws", id)
		}
		seen[id] = true
	}
}
