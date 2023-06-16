package harness

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/swap"
	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/load"
	"github.com/helmedeiros/markup-svc/internal/markup"
	"github.com/helmedeiros/markup-svc/internal/snapshot"
)

type stubDecider struct{}

func (stubDecider) Decide(_ context.Context, _ markup.Request) (markup.Decision, error) {
	return markup.Decision{Rule: "bench"}, nil
}

type benchBodyLoader struct {
	modelVersion string
}

func (b *benchBodyLoader) Supports(ct string) bool { return ct == "text/csv" || ct == "application/json" }

func (b *benchBodyLoader) Load(ct string, body []byte) (markup.Decider, httpapi.ReloadResult, error) {
	switch ct {
	case "text/csv":
		rules, err := load.FromCSV(bytes.NewReader(body))
		if err != nil {
			return nil, httpapi.ReloadResult{}, &markup.InvalidRuleSetError{Path: "<body>", Err: err}
		}
		return stubDecider{}, httpapi.ReloadResult{RuleCount: len(rules), ModelVersion: b.modelVersion}, nil
	case "application/json":
		snap, err := snapshot.Read(bytes.NewReader(body))
		if err != nil {
			return nil, httpapi.ReloadResult{}, &markup.InvalidRuleSetError{Path: "<body>", Err: err}
		}
		return stubDecider{}, httpapi.ReloadResult{RuleCount: len(snap.EngineSnapshot.Rules), ModelVersion: snap.ModelVersion}, nil
	}
	return nil, httpapi.ReloadResult{}, fmt.Errorf("unsupported content type %q", ct)
}

func benchHandler(b *testing.B, body httpapi.ReloadBodyLoader, tmpRulesPath string) http.Handler {
	b.Helper()
	holder := swap.New(stubDecider{})
	loader := func() (markup.Decider, httpapi.ReloadResult, error) {
		f, err := os.Open(tmpRulesPath)
		if err != nil {
			return nil, httpapi.ReloadResult{}, err
		}
		defer f.Close()
		rules, err := load.FromCSV(f)
		if err != nil {
			return nil, httpapi.ReloadResult{}, err
		}
		return stubDecider{}, httpapi.ReloadResult{RuleCount: len(rules), ModelVersion: "bench"}, nil
	}
	if body != nil {
		return httpapi.Reload(holder, loader, httpapi.WithReloadBodyLoader(body))
	}
	return httpapi.Reload(holder, loader)
}

func writeRulesCSV(b *testing.B, count int) string {
	b.Helper()
	dir := b.TempDir()
	path := filepath.Join(dir, "rules.csv")
	var sb strings.Builder
	sb.WriteString("name,condition,factor,priority\n")
	for i := 0; i < count; i++ {
		fmt.Fprintf(&sb, "rule_%d,customer_tier == 'tier_%d',1.0%d,%d\n", i, i, i%9, i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		b.Fatalf("write rules: %v", err)
	}
	return path
}

func readBytes(b *testing.B, path string) []byte {
	b.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		b.Fatalf("read %s: %v", path, err)
	}
	return data
}

func BenchmarkReload_EmptyBody(b *testing.B) {
	rulesPath := writeRulesCSV(b, 100)
	body := &benchBodyLoader{modelVersion: "bench"}
	h := benchHandler(b, body, rulesPath)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}

func BenchmarkReload_CSVBody_100Rules(b *testing.B) {
	rulesPath := writeRulesCSV(b, 100)
	bodyBytes := readBytes(b, rulesPath)
	body := &benchBodyLoader{modelVersion: "bench"}
	h := benchHandler(b, body, rulesPath)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/admin/reload", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "text/csv")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}

func BenchmarkReload_CSVBody_10kRules(b *testing.B) {
	rulesPath := writeRulesCSV(b, 10000)
	bodyBytes := readBytes(b, rulesPath)
	body := &benchBodyLoader{modelVersion: "bench"}
	h := benchHandler(b, body, rulesPath)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/admin/reload", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "text/csv")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}
