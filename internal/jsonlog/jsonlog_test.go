package jsonlog_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/jsonlog"
)

func TestLogger_Info_EmitsShapeAndAttrs(t *testing.T) {
	var buf bytes.Buffer
	l := jsonlog.New(&buf)
	l.Info("markup-server.boot", map[string]any{
		"listen":  ":8080",
		"rules":   3,
		"adapter": "inmemory",
	})

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v ; raw=%q", err, buf.String())
	}
	if got["level"] != "info" {
		t.Errorf("level=%v want info", got["level"])
	}
	if got["msg"] != "markup-server.boot" {
		t.Errorf("msg=%v want markup-server.boot", got["msg"])
	}
	attrs := got["attrs"].(map[string]any)
	if attrs["listen"] != ":8080" || attrs["rules"].(float64) != 3 || attrs["adapter"] != "inmemory" {
		t.Errorf("attrs mismatch: %v", attrs)
	}
	if _, ok := got["time"].(string); !ok {
		t.Errorf("time field missing or not string: %v", got["time"])
	}
}

func TestLogger_ConcurrentSerialisesEntries(t *testing.T) {
	var buf bytes.Buffer
	l := jsonlog.New(&buf)
	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func(i int) {
			l.Info("concurrent", map[string]any{"i": i})
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 50; i++ {
		<-done
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 50 {
		t.Fatalf("got %d lines, want 50", len(lines))
	}
	for _, line := range lines {
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Errorf("non-JSON line: %q (%v)", line, err)
		}
	}
}
