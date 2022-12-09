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
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/helmedeiros/markup-svc/internal/decider/firstmatch"
	"github.com/helmedeiros/markup-svc/internal/decider/indexed"
	"github.com/helmedeiros/markup-svc/internal/decider/inmemory"
	"github.com/helmedeiros/markup-svc/internal/decider/priority"
	"github.com/helmedeiros/markup-svc/internal/decider/router"
	"github.com/helmedeiros/markup-svc/internal/decider/swap"
	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/load"
	"github.com/helmedeiros/markup-svc/internal/markup"
	mkotel "github.com/helmedeiros/markup-svc/internal/observability/otel"
	"github.com/helmedeiros/markup-svc/internal/snapshot"
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
	rulesPath := fs.String("rules", "", "path to CSV rule file (see ADR-0002); mutually exclusive with --snapshot")
	snapshotPath := fs.String("snapshot", "", "path to snapshot JSON (see ADR-0007); cold-starts the indexed adapter; mutually exclusive with --rules")
	listen := fs.String("listen", ":8080", "HTTP listen address")
	modelVersion := fs.String("model", "v1", "model version tag carried on every Decision (overridden by snapshot's ModelVersion when --snapshot is set)")
	adapter := fs.String("adapter", "inmemory", "Decider adapter: inmemory|firstmatch|priority|indexed (ignored when --snapshot is set; snapshots are indexed-only)")
	otelEnabled := fs.Bool("otel-enabled", false, "wrap Decide calls in OpenTelemetry spans (see ADR-0009); spans emit via the no-op tracer unless an exporter is configured via OTel SDK env vars")
	var routeSpecs routeFlagList
	fs.Var(&routeSpecs, "route", "repeatable; format: model:variant:type:path where type is rules|snapshot. When set, mutually exclusive with --rules/--snapshot. See ADR-0011.")
	policyName := fs.String("policy", "hash-correlation", "routing policy when multiple --route flags are set: hash-correlation|default")
	if err := fs.Parse(args); err != nil {
		return err
	}

	routerMode := len(routeSpecs) > 0
	if routerMode && (*rulesPath != "" || *snapshotPath != "") {
		return fmt.Errorf("--route is mutually exclusive with --rules/--snapshot")
	}
	if !routerMode && *rulesPath != "" && *snapshotPath != "" {
		return fmt.Errorf("--rules and --snapshot are mutually exclusive")
	}
	if !routerMode && *rulesPath == "" && *snapshotPath == "" {
		return fmt.Errorf("one of --rules, --snapshot, or --route is required")
	}

	var tracer trace.Tracer
	if *otelEnabled {
		tracer = otel.Tracer("github.com/helmedeiros/markup-svc/cmd/markup-server")
	}

	var (
		handler     http.Handler
		ruleCount   int
		bootSource  string
		bootAdapter string
		bootModel   = *modelVersion
	)
	if routerMode {
		policy, perr := pickRouterPolicy(*policyName)
		if perr != nil {
			return perr
		}
		routes, total, perr := buildRoutes(routeSpecs, stderr)
		if perr != nil {
			return perr
		}
		handler = wireRouterHandler(router.New(routes, policy), tracer)
		ruleCount = total
		bootSource = fmt.Sprintf("%d routes (policy=%s)", len(routes), *policyName)
		bootAdapter = "router"
		bootModel = fmt.Sprintf("multi(%d)", len(routes))
	} else {
		var loader httpapi.Loader
		if *snapshotPath != "" {
			loader = snapshotLoader(*snapshotPath, stderr)
			bootSource = *snapshotPath
			bootAdapter = "indexed"
		} else {
			loader = rulesLoader(*rulesPath, *adapter, *modelVersion, stderr)
			bootSource = *rulesPath
			bootAdapter = *adapter
		}
		h, initialResult, err := wireTracedHandler(loader, tracer)
		if err != nil {
			return err
		}
		handler = h
		ruleCount = initialResult.RuleCount
		bootModel = initialResult.ModelVersion
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		fmt.Fprintf(stdout, "markup-server: listening on %s (%d rules, model %s, adapter %s, source %s)\n",
			*listen, ruleCount, bootModel, bootAdapter, bootSource)
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

// wireHandler runs loader once for the initial Decider, wraps it in a
// swap.Decider holder, mounts /decide on the holder, and mounts
// /admin/reload on the same holder + loader so hot reloads re-run
// the same load path against the current file contents. The
// returned http.Handler is the production wiring -- same shape used
// by tests so they exercise the real seam.
func wireHandler(loader httpapi.Loader) (http.Handler, httpapi.ReloadResult, error) {
	return wireTracedHandler(loader, nil)
}

// routeFlagList accumulates repeated --route flag values into a slice
// so cmd/markup-server can specify multi-route deployments at boot.
type routeFlagList []string

// String implements flag.Value.
func (r *routeFlagList) String() string { return strings.Join(*r, ",") }

// Set implements flag.Value -- appends each --route occurrence.
func (r *routeFlagList) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// wireRouterHandler mounts /decide on a Router behind the optional
// OTel decorator and the correlation-ID middleware. Router mode does
// not mount /admin/reload because per-route reload is a separate
// design (each route would need its own swap.Decider holder and a
// reload contract that names which route to refresh). Operators
// running multi-route deployments today restart the process to swap
// rule sets; per-route hot reload lands in a follow-up.
func wireRouterHandler(r *router.Router, tracer trace.Tracer) http.Handler {
	var decideDecider markup.Decider = r
	if tracer != nil {
		decideDecider = mkotel.Wrap(r, tracer)
	}
	mux := http.NewServeMux()
	mux.Handle("/decide", httpapi.Decide(decideDecider))
	return httpapi.WithCorrelationID(mux)
}

// pickRouterPolicy maps the --policy flag name to a router.Policy.
// Unknown names fail boot fast (mirrors the --adapter flag posture).
func pickRouterPolicy(name string) (router.Policy, error) {
	switch name {
	case "hash-correlation":
		return router.HashCorrelationPolicy{}, nil
	case "default":
		return router.DefaultPolicy{}, nil
	default:
		return nil, fmt.Errorf("unknown --policy %q (want one of: hash-correlation, default)", name)
	}
}

// buildRoutes parses every --route flag value and builds the
// corresponding router.Route. Returns the total rule count across
// all routes so the startup log line accurately reflects the boot
// inventory.
func buildRoutes(specs routeFlagList, stderr io.Writer) ([]router.Route, int, error) {
	out := make([]router.Route, 0, len(specs))
	totalRules := 0
	for _, raw := range specs {
		parts := strings.SplitN(raw, ":", 4)
		if len(parts) != 4 {
			return nil, 0, fmt.Errorf("invalid --route %q (want model:variant:type:path)", raw)
		}
		model, variant, srcType, srcPath := parts[0], parts[1], parts[2], parts[3]
		if model == "" {
			return nil, 0, fmt.Errorf("--route %q: model field is required", raw)
		}
		if srcPath == "" {
			return nil, 0, fmt.Errorf("--route %q: path field is required", raw)
		}
		var loader httpapi.Loader
		switch srcType {
		case "rules":
			loader = rulesLoader(srcPath, "inmemory", model, stderr)
		case "snapshot":
			loader = snapshotLoader(srcPath, stderr)
		default:
			return nil, 0, fmt.Errorf("--route %q: type must be rules|snapshot, got %q", raw, srcType)
		}
		decider, result, err := loader()
		if err != nil {
			return nil, 0, fmt.Errorf("--route %q: %w", raw, err)
		}
		out = append(out, router.Route{
			ModelVersion: model,
			Variant:      variant,
			Decider:      decider,
		})
		totalRules += result.RuleCount
	}
	return out, totalRules, nil
}

// wireTracedHandler is wireHandler with an optional OpenTelemetry
// tracer. When tracer is non-nil the otel decorator wraps the
// swap.Decider holder (not the other way round): the /decide call
// chain is traced -> holder -> inner so a hot reload that swaps
// holder's inner still flows through the traced layer and continues
// emitting spans. The /admin/reload route keeps calling holder.Swap
// directly -- swaps are administrative, not user traffic.
func wireTracedHandler(loader httpapi.Loader, tracer trace.Tracer) (http.Handler, httpapi.ReloadResult, error) {
	initial, result, err := loader()
	if err != nil {
		return nil, httpapi.ReloadResult{}, err
	}
	holder := swap.New(initial)
	var decideDecider markup.Decider = holder
	if tracer != nil {
		decideDecider = mkotel.Wrap(holder, tracer)
	}
	mux := http.NewServeMux()
	mux.Handle("/decide", httpapi.Decide(decideDecider))
	mux.Handle("/admin/reload", httpapi.Reload(holder, loader))
	return httpapi.WithCorrelationID(mux), result, nil
}

// snapshotLoader is the boot-time-capturing loader for the --snapshot
// path. Reads the file fresh on every call (boot + reload), validates
// FormatVersion via snapshot.Read, and returns the rebuilt indexed
// Decider along with rule count + ModelVersion for the response body.
// Errors are logged to stderr so operators see the underlying detail
// the handler keeps out of the response. See ADR-0007 and ADR-0008.
func snapshotLoader(path string, stderr io.Writer) httpapi.Loader {
	return func() (markup.Decider, httpapi.ReloadResult, error) {
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(stderr, "snapshot loader: open %q: %v\n", path, err)
			return nil, httpapi.ReloadResult{}, fmt.Errorf("open snapshot %q: %w", path, err)
		}
		defer f.Close()
		snap, err := snapshot.Read(f)
		if err != nil {
			fmt.Fprintf(stderr, "snapshot loader: read %q: %v\n", path, err)
			return nil, httpapi.ReloadResult{}, fmt.Errorf("read snapshot %q: %w", path, err)
		}
		decider, err := snapshot.LoadIntoIndexedDecider(snap)
		if err != nil {
			fmt.Fprintf(stderr, "snapshot loader: load %q: %v\n", path, err)
			return nil, httpapi.ReloadResult{}, fmt.Errorf("load snapshot %q: %w", path, err)
		}
		return decider, httpapi.ReloadResult{
			RuleCount:    len(snap.EngineSnapshot.Rules),
			ModelVersion: snap.ModelVersion,
		}, nil
	}
}

// rulesLoader is the boot-time-capturing loader for the --rules path.
// Reads the CSV fresh on every call, builds the configured adapter,
// and returns rule count + the boot-time ModelVersion. The
// ModelVersion comes from the --model flag because the CSV does not
// carry one; operators who want a tag bump through reload restart
// the process. See ADR-0008.
func rulesLoader(path, adapter, modelVersion string, stderr io.Writer) httpapi.Loader {
	return func() (markup.Decider, httpapi.ReloadResult, error) {
		rules, err := loadRulesFromFile(path)
		if err != nil {
			fmt.Fprintf(stderr, "rules loader: %v\n", err)
			return nil, httpapi.ReloadResult{}, err
		}
		decider, err := buildDecider(adapter, rules, modelVersion)
		if err != nil {
			fmt.Fprintf(stderr, "rules loader: build decider: %v\n", err)
			return nil, httpapi.ReloadResult{}, fmt.Errorf("build decider: %w", err)
		}
		return decider, httpapi.ReloadResult{
			RuleCount:    len(rules),
			ModelVersion: modelVersion,
		}, nil
	}
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
	case "priority":
		return priority.NewFromRules(rules, modelVersion)
	case "indexed":
		return indexed.NewFromRules(rules, modelVersion)
	default:
		return nil, fmt.Errorf("unknown adapter %q (want one of: inmemory, firstmatch, priority, indexed)", adapter)
	}
}
