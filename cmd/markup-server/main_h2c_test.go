package main

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// TestE2EH2CServerAcceptsHTTP2PriorKnowledge confirms the server
// negotiates HTTP/2 over plaintext with a client that sends HTTP/2
// frames directly. The gateway will use this path in production.
func TestE2EH2CServerAcceptsHTTP2PriorKnowledge(t *testing.T) {
	rulesPath := filepath.Join(t.TempDir(), "rules.csv")
	if err := writeFile(t, rulesPath, "name,condition,factor,priority\nenterprise,customer_tier == 'enterprise',1.15,10\n"); err != nil {
		t.Fatal(err)
	}
	loader := rulesLoader(rulesPath, "inmemory", "v0-h2c", io.Discard)
	handler, _, err := wireTracedHandler(loader, nil, nil, guardrailsWire{}, metricsWiring{}, nil, nil, false, 1.0, 0, "")
	if err != nil {
		t.Fatalf("wireTracedHandler: %v", err)
	}

	srv := httptest.NewUnstartedServer(handler)
	srv.Config.Handler = h2c.NewHandler(handler, &http2.Server{})
	srv.Start()
	t.Cleanup(srv.Close)

	client := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/decide",
		strings.NewReader(`{"customer_tier":"enterprise"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.ProtoMajor != 2 {
		t.Errorf("ProtoMajor = %d, want 2 (h2c)", resp.ProtoMajor)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
