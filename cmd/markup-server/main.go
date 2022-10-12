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

	"github.com/helmedeiros/markup-svc/internal/decider/inmemory"
	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/load"
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
	if err := fs.Parse(args); err != nil {
		return err
	}

	rules, err := loadRulesFromFile(*rulesPath)
	if err != nil {
		return err
	}
	decider, err := inmemory.NewFromRules(rules, *modelVersion)
	if err != nil {
		return fmt.Errorf("build decider: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/decide", httpapi.Decide(decider))
	srv := &http.Server{
		Addr:              *listen,
		Handler:           httpapi.WithCorrelationID(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		fmt.Fprintf(stdout, "markup-server: listening on %s (%d rules, model %s)\n", *listen, len(rules), *modelVersion)
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
