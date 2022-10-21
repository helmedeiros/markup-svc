// Package main is the markup-svc HTTP server entry point. Boots an
// inmemory.Decider from a CSV rule file, mounts the /decide handler
// behind the correlation-ID middleware, and serves until SIGINT or
// SIGTERM triggers a graceful shutdown. The main() func is a thin
// glue over run() so the wiring is exercised by tests in cmd/markup-
// server_test.go.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/helmedeiros/markup-svc/internal/decider/firstmatch"
	"github.com/helmedeiros/markup-svc/internal/decider/inmemory"
	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/load"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "markup-server: %v\n", err)
		os.Exit(1)
	}
}

// run wires the markup server. Separated from main so tests can drive
// it with a cancellable ctx, captured stdout/stderr, and synthetic
// args without spawning a real process.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("markup-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rulesPath := fs.String("rules", "rules.csv", "path to CSV rule file (see ADR-0002)")
	listen := fs.String("listen", ":8080", "HTTP listen address")
	modelVersion := fs.String("model", "v1", "model version tag carried on every Decision")
	adapter := fs.String("adapter", "inmemory", "Decider adapter: inmemory|firstmatch")
	if err := fs.Parse(args); err != nil {
		return err
	}

	rules, err := loadRulesFromFile(*rulesPath)
	if err != nil {
		return err
	}
	handler, err := buildHandler(*adapter, rules, *modelVersion)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		fmt.Fprintf(stdout, "markup-server: listening on %s (%d rules, model %s, adapter %s)\n", *listen, len(rules), *modelVersion, *adapter)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case <-ctx.Done():
		fmt.Fprintln(stdout, "markup-server: shutting down")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-serverErr:
		return err
	}
}

func loadRulesFromFile(path string) ([]load.Rule, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open rules %q: %w", path, err)
	}
	defer f.Close()
	rules, err := load.FromCSV(f)
	if err != nil {
		return nil, fmt.Errorf("parse rules %q: %w", path, err)
	}
	return rules, nil
}

// buildHandler is the wiring seam between loaded rules and the served
// HTTP handler: Decider behind /decide behind the correlation ID
// middleware. The Decider is selected by adapter name; unknown names
// surface as an error so a typo on the --adapter flag fails boot
// fast rather than silently picking a default. Exposed at package
// scope so end-to-end tests can drive the full stack via httptest
// without spawning a real process.
func buildHandler(adapter string, rules []load.Rule, modelVersion string) (http.Handler, error) {
	decider, err := buildDecider(adapter, rules, modelVersion)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/decide", httpapi.Decide(decider))
	return httpapi.WithCorrelationID(mux), nil
}

// buildDecider dispatches on adapter name. Each branch is a single
// call into the matching adapter package's NewFromRules; the
// dispatcher itself owns no behaviour beyond the name -> constructor
// mapping.
func buildDecider(adapter string, rules []load.Rule, modelVersion string) (markup.Decider, error) {
	switch adapter {
	case "inmemory":
		return inmemory.NewFromRules(rules, modelVersion)
	case "firstmatch":
		return firstmatch.NewFromRules(rules, modelVersion)
	default:
		return nil, fmt.Errorf("unknown adapter %q (want one of: inmemory, firstmatch)", adapter)
	}
}
