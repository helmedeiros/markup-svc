// Package main is the markup-svc HTTP server entry point. Boots an
// inmemory.Decider from a CSV rule file, mounts the /decide handler
// behind the correlation-ID middleware, and serves until SIGINT or
// SIGTERM triggers a graceful shutdown. The main() func is a thin
// glue over run() so the wiring is exercised by tests in cmd/markup-
// server_test.go.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/helmedeiros/markup-svc/internal/decider/firstmatch"
	"github.com/helmedeiros/markup-svc/internal/decider/guardrails"
	"github.com/helmedeiros/markup-svc/internal/decider/indexed"
	"github.com/helmedeiros/markup-svc/internal/decider/inmemory"
	"github.com/helmedeiros/markup-svc/internal/decider/priority"
	"github.com/helmedeiros/markup-svc/internal/decider/router"
	"github.com/helmedeiros/markup-svc/internal/decider/swap"
	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/jsonlog"
	"github.com/helmedeiros/markup-svc/internal/load"
	"github.com/helmedeiros/markup-svc/internal/markup"
	mkmetrics "github.com/helmedeiros/markup-svc/internal/observability/metrics"
	mkprom "github.com/helmedeiros/markup-svc/internal/observability/metrics/prom"
	mkotel "github.com/helmedeiros/markup-svc/internal/observability/otel"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
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
	otelEnabled := fs.Bool("otel-enabled", false, "bootstrap the OTel SDK + emit three-layer Decide spans (markup.decider.decide / markup.guardrails.check / markup.engine.evaluate) and accept incoming W3C traceparent so spans join the caller's trace; reads OTEL_EXPORTER_OTLP_ENDPOINT etc. per the OTel SDK conventions (see ADR-0009, ADR-0016, ADR-0017)")
	metricsEnabled := fs.Bool("metrics-enabled", false, "wrap Decide with the metrics decorator (ADR-0010) writing to a Prometheus Sink + mount /metrics on the same listener for scraping (see ADR-0019)")
	diagnoseMode := fs.String("diagnose", "on", "rule-set diagnose mode at boot: on (fail boot on errors), warn (log issues + continue), off (skip). Mounts GET /admin/diagnose on the same listener. See ADR-0025.")
	var routeSpecs routeFlagList
	fs.Var(&routeSpecs, "route", "repeatable; format: model:variant:type:path where type is rules|snapshot. When set, mutually exclusive with --rules/--snapshot. See ADR-0011.")
	policyName := fs.String("policy", "hash-correlation", "routing policy when multiple --route flags are set: hash-correlation|default")
	minFactor := fs.Float64("min-factor", 0, "guardrails: minimum allowed Decision.MarkupFactor (closed interval with --max-factor); see ADR-0014")
	maxFactor := fs.Float64("max-factor", 0, "guardrails: maximum allowed Decision.MarkupFactor (closed interval with --min-factor); see ADR-0014")
	allowedCountries := fs.String("allowed-countries", "", "guardrails: comma-separated list of allowed Request.Country values (e.g., BR,DE,FR); see ADR-0014")
	requiredFields := fs.String("required-fields", "", "guardrails: comma-separated list of Request fields that must be non-empty (e.g., customer_tier,country); see ADR-0014")
	guardrailsAdmin := fs.Bool("guardrails-admin", false, "mount POST/GET /admin/guardrails for hot-replacing the active rule set without restart (see ADR-0015); enables Holder-based wiring with a ~10 ns lock-pair per Decide")
	if err := fs.Parse(args); err != nil {
		return err
	}
	guardrailRules, gerr := buildGuardrailRules(fs, *minFactor, *maxFactor, *allowedCountries, *requiredFields)
	if gerr != nil {
		return gerr
	}
	guardWiring := buildGuardrailsWiring(*guardrailsAdmin, guardrailRules, stderr)

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
		t, shutdown, err := mkotel.Bootstrap(ctx, "github.com/helmedeiros/markup-svc/cmd/markup-server")
		if err != nil {
			return fmt.Errorf("otel bootstrap: %w", err)
		}
		tracer = t
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shutdown(shutdownCtx)
		}()
	}

	var metricsWire metricsWiring
	if *metricsEnabled {
		sink, h := mkprom.New()
		metricsWire = metricsWiring{sink: sink, handler: h}
	}

	log := jsonlog.New(stdout)

	var diagnoseFn httpapi.DiagnoseFn
	if *rulesPath != "" {
		rp := *rulesPath
		diagnoseFn = func() (markup.Diagnosis, error) {
			rules, err := loadRulesFromFile(rp)
			if err != nil {
				return markup.Diagnosis{}, err
			}
			return load.Diagnose(rules), nil
		}
		if *diagnoseMode != "off" {
			d, derr := diagnoseFn()
			if derr != nil {
				return fmt.Errorf("diagnose: %w", derr)
			}
			for _, issue := range d.Issues {
				attrs := map[string]any{"kind": string(issue.Kind), "detail": issue.Detail}
				if issue.Rule != "" {
					attrs["rule"] = issue.Rule
				}
				switch issue.Severity {
				case markup.SeverityError:
					log.Error("markup-server.diagnose", attrs)
				case markup.SeverityWarning:
					log.Warn("markup-server.diagnose", attrs)
				}
			}
			if *diagnoseMode == "on" && !d.IsHealthy() {
				return fmt.Errorf("diagnose: %d error issue(s) in rule set; fix or pass --diagnose=warn to start anyway", len(d.Errors()))
			}
		}
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
		routes, holders, total, perr := buildRoutes(routeSpecs, stderr)
		if perr != nil {
			return perr
		}
		handler = wireRouterHandler(router.New(routes, policy), tracer, holders, guardWiring, metricsWire, log)
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
		body := newBodyLoader(bootAdapter, *modelVersion, stderr)
		h, initialResult, err := wireTracedHandler(loader, body, tracer, guardWiring, metricsWire, log, diagnoseFn)
		if err != nil {
			return err
		}
		handler = h
		ruleCount = initialResult.RuleCount
		bootModel = initialResult.ModelVersion
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           h2c.NewHandler(handler, &http2.Server{}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() {
		log.Info("markup-server.boot", map[string]any{
			"listen":  *listen,
			"rules":   ruleCount,
			"model":   bootModel,
			"adapter": bootAdapter,
			"source":  bootSource,
		})
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case <-ctx.Done():
		log.Info("markup-server.shutdown", nil)
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
		return nil, &markup.InvalidRuleSetError{Path: path, Err: err}
	}
	return rules, nil
}

// readyState is the package-level atomic flag the /readyz handler
// reads on every probe. Set to 1 after the initial Decider
// construction succeeds. atomic.Int32 (vs atomic.Bool) for the
// project's Go 1.18 baseline.
var readyState int32

func markReady() { atomic.StoreInt32(&readyState, 1) }

func isReady() (string, bool) {
	if atomic.LoadInt32(&readyState) == 1 {
		return "", true
	}
	return "decider not built", false
}

// wireHandler runs loader once for the initial Decider, wraps it in a
// swap.Decider holder, mounts /decide on the holder, and mounts
// /admin/reload on the same holder + loader so hot reloads re-run
// the same load path against the current file contents. /healthz
// and /readyz are mounted alongside per ADR-0013. The returned
// http.Handler is the production wiring -- same shape used by
// tests so they exercise the real seam.
func wireHandler(loader httpapi.Loader) (http.Handler, httpapi.ReloadResult, error) {
	return wireTracedHandler(loader, nil, nil, guardrailsWire{}, metricsWiring{}, nil, nil)
}

// metricsWiring bundles the optional Prometheus metrics decorator
// + the matching /metrics HTTP handler. The zero value disables both:
// the decorator is not wrapped and /metrics is not mounted. See
// ADR-0019 in this repo + ADR-0003 in pricing-observability for the
// scrape contract.
type metricsWiring struct {
	sink    mkmetrics.Sink
	handler http.Handler
}

// guardrailsWire bundles the optional guardrails-decorator layer and
// the optional admin-mount closure so the wire functions take one
// parameter regardless of which mode (no guardrails / immutable
// boot-flag rules / Holder + admin) is active. The zero value is
// "no guardrails, no admin endpoint" -- the binary serves with zero
// per-Decide overhead from this layer.
//
// wrap, when non-nil, is applied to the inner Decider between the
// holder/router and the OTel decorator. mountAdmin, when non-nil, is
// called with the same mux that mounts /decide so the admin endpoint
// lives alongside the rest of the routes (and inherits the same
// correlation-ID middleware).
type guardrailsWire struct {
	wrap       func(markup.Decider) markup.Decider
	mountAdmin func(mux *http.ServeMux, wrap func(http.Handler) http.Handler)
}

// buildGuardrailsWiring picks the active guardrails mode from the
// flags and assembles the wire helper.
//
//   - adminEnabled && no rules: Holder starts empty; operators set
//     the configuration via POST /admin/guardrails.
//   - adminEnabled && rules: Holder starts with the boot-flag rules;
//     operators can still POST a replacement at runtime.
//   - !adminEnabled && rules: immutable guardrails.New decorator,
//     zero lock overhead per ADR-0014.
//   - !adminEnabled && no rules: zero value of guardrailsWire; the
//     decorator is not mounted.
func buildGuardrailsWiring(adminEnabled bool, rules []guardrails.Rule, errLog io.Writer) guardrailsWire {
	if adminEnabled {
		holder := guardrails.NewHolder(rules...)
		return guardrailsWire{
			wrap: holder.Wrap,
			mountAdmin: func(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
				h := http.Handler(httpapi.GuardrailsAdmin(holder, errLog))
				if wrap != nil {
					h = wrap(h)
				}
				mux.Handle("/admin/guardrails", h)
			},
		}
	}
	if len(rules) == 0 {
		return guardrailsWire{}
	}
	return guardrailsWire{
		wrap: func(inner markup.Decider) markup.Decider {
			return guardrails.New(inner, rules...)
		},
	}
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
// OTel decorator and the correlation-ID middleware. When holders is
// non-empty, /admin/reload is also mounted as a route-aware handler:
// POST /admin/reload with body {"model_version":"v1"} reloads the
// named route's inner Decider through its dedicated swap.Decider
// holder. When holders is nil (legacy callers that did not build
// per-route reload infrastructure), /admin/reload is not mounted
// and POSTs return 404. See ADR-0011's follow-up commit.
func wireRouterHandler(r *router.Router, tracer trace.Tracer, holders []routeHolder, gw guardrailsWire, mw metricsWiring, log *jsonlog.Logger) http.Handler {
	var decideDecider markup.Decider = r
	if tracer != nil {
		decideDecider = mkotel.Wrap(decideDecider, tracer, mkotel.WithSpanName("markup.engine.evaluate"))
	}
	if gw.wrap != nil {
		decideDecider = gw.wrap(decideDecider)
		if tracer != nil {
			decideDecider = mkotel.Wrap(decideDecider, tracer, mkotel.WithSpanName("markup.guardrails.check"))
		}
	}
	if tracer != nil {
		decideDecider = mkotel.Wrap(decideDecider, tracer, mkotel.WithSpanKind(trace.SpanKindServer))
	}
	// metrics decorator goes OUTERMOST so Duration captures the
	// full stack including tracing overhead (per ADR-0010's
	// recommended order).
	if mw.sink != nil {
		decideDecider = mkmetrics.Wrap(decideDecider, mw.sink)
	}
	mux := http.NewServeMux()
	mux.Handle("/decide", httpapi.Decide(decideDecider))
	if len(holders) > 0 {
		mux.Handle("/admin/reload", httpapi.WithAdminSpan(tracer, "markup.admin.reload", routeReloadHandler(holders)))
	}
	if gw.mountAdmin != nil {
		gw.mountAdmin(mux, func(h http.Handler) http.Handler {
			return httpapi.WithAdminSpan(tracer, "markup.admin.guardrails", h)
		})
	}
	if mw.handler != nil {
		mux.Handle("/metrics", mw.handler)
	}
	mux.Handle("/healthz", httpapi.Healthz())
	mux.Handle("/readyz", httpapi.Readyz(isReady))
	markReady()
	return httpapi.WithCorrelationID(httpapi.WithTraceContext(httpapi.WithAccessLog(log, mux)))
}

// routeHolder bundles a route's ModelVersion with its swap.Decider
// holder and the loader closure that rebuilds the route from disk.
// The Router holds the swap.Decider as the Route's Decider; reload
// swaps the inner without touching the Router or the swap reference.
type routeHolder struct {
	modelVersion string
	holder       *swap.Decider
	loader       httpapi.Loader
}

// routeReloadHandler implements the route-aware admin endpoint. POST
// {"model_version":"v1"} runs the v1 loader and swaps the v1 holder.
// 400 on malformed body, 404 on unknown model_version, 500 on loader
// failure (in which case the old Decider keeps serving), 405 on
// non-POST with Allow: POST. Mirrors httpapi.Reload's error posture
// but parameterized by route name so a single endpoint serves every
// route in the deployment.
func routeReloadHandler(holders []routeHolder) http.Handler {
	byName := make(map[string]routeHolder, len(holders))
	for _, h := range holders {
		byName[h.modelVersion] = h
	}
	type req struct {
		ModelVersion string `json:"model_version"`
	}
	type resp struct {
		RuleCount    int    `json:"rule_count"`
		ModelVersion string `json:"model_version"`
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
			return
		}
		var body req
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ModelVersion == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "body must be JSON {\"model_version\":\"...\"}"})
			return
		}
		route, ok := byName[body.ModelVersion]
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown model_version"})
			return
		}
		next, result, err := route.loader()
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "reload failed"})
			return
		}
		route.holder.Swap(next)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp{RuleCount: result.RuleCount, ModelVersion: result.ModelVersion})
	})
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
// corresponding router.Route. Each Route's Decider is wrapped in its
// own swap.Decider holder so the route-aware reload handler can swap
// the inner without disrupting in-flight requests on other routes.
// Returns the routes (with holders as their inner), the per-route
// loaders+holders for /admin/reload wiring, and the total rule count.
func buildRoutes(specs routeFlagList, stderr io.Writer) ([]router.Route, []routeHolder, int, error) {
	routes := make([]router.Route, 0, len(specs))
	holders := make([]routeHolder, 0, len(specs))
	totalRules := 0
	for _, raw := range specs {
		parts := strings.SplitN(raw, ":", 4)
		if len(parts) != 4 {
			return nil, nil, 0, fmt.Errorf("invalid --route %q (want model:variant:type:path)", raw)
		}
		model, variant, srcType, srcPath := parts[0], parts[1], parts[2], parts[3]
		if model == "" {
			return nil, nil, 0, fmt.Errorf("--route %q: model field is required", raw)
		}
		if srcPath == "" {
			return nil, nil, 0, fmt.Errorf("--route %q: path field is required", raw)
		}
		var loader httpapi.Loader
		switch srcType {
		case "rules":
			loader = rulesLoader(srcPath, "inmemory", model, stderr)
		case "snapshot":
			loader = snapshotLoader(srcPath, stderr)
		default:
			return nil, nil, 0, fmt.Errorf("--route %q: type must be rules|snapshot, got %q", raw, srcType)
		}
		decider, result, err := loader()
		if err != nil {
			return nil, nil, 0, fmt.Errorf("--route %q: %w", raw, err)
		}
		holder := swap.New(decider)
		routes = append(routes, router.Route{
			ModelVersion: model,
			Variant:      variant,
			Decider:      holder,
		})
		holders = append(holders, routeHolder{modelVersion: model, holder: holder, loader: loader})
		totalRules += result.RuleCount
	}
	return routes, holders, totalRules, nil
}

// wireTracedHandler is wireHandler with an optional OpenTelemetry
// tracer. When tracer is non-nil the otel decorator wraps the
// swap.Decider holder (not the other way round): the /decide call
// chain is traced -> holder -> inner so a hot reload that swaps
// holder's inner still flows through the traced layer and continues
// emitting spans. The /admin/reload route keeps calling holder.Swap
// directly -- swaps are administrative, not user traffic.
func wireTracedHandler(loader httpapi.Loader, body httpapi.ReloadBodyLoader, tracer trace.Tracer, gw guardrailsWire, mw metricsWiring, log *jsonlog.Logger, diagnoseFn httpapi.DiagnoseFn) (http.Handler, httpapi.ReloadResult, error) {
	initial, result, err := loader()
	if err != nil {
		return nil, httpapi.ReloadResult{}, err
	}
	holder := swap.New(initial)
	var decideDecider markup.Decider = holder
	if tracer != nil {
		decideDecider = mkotel.Wrap(decideDecider, tracer, mkotel.WithSpanName("markup.engine.evaluate"))
	}
	if gw.wrap != nil {
		decideDecider = gw.wrap(decideDecider)
		if tracer != nil {
			decideDecider = mkotel.Wrap(decideDecider, tracer, mkotel.WithSpanName("markup.guardrails.check"))
		}
	}
	if tracer != nil {
		decideDecider = mkotel.Wrap(decideDecider, tracer, mkotel.WithSpanKind(trace.SpanKindServer))
	}
	if mw.sink != nil {
		decideDecider = mkmetrics.Wrap(decideDecider, mw.sink)
	}
	mux := http.NewServeMux()
	mux.Handle("/decide", httpapi.Decide(decideDecider))
	var reloadH http.Handler
	reloadOpts := []httpapi.ReloadOption{}
	if diagnoseFn != nil {
		reloadOpts = append(reloadOpts, httpapi.WithReloadDiagnose(diagnoseFn))
	}
	if body != nil {
		reloadOpts = append(reloadOpts, httpapi.WithReloadBodyLoader(body))
	}
	reloadH = httpapi.Reload(holder, loader, reloadOpts...)
	mux.Handle("/admin/reload", httpapi.WithAdminSpan(tracer, "markup.admin.reload", reloadH))
	if diagnoseFn != nil {
		mux.Handle("/admin/diagnose", httpapi.WithAdminSpan(tracer, "markup.admin.diagnose", httpapi.Diagnose(diagnoseFn)))
	}
	if gw.mountAdmin != nil {
		gw.mountAdmin(mux, func(h http.Handler) http.Handler {
			return httpapi.WithAdminSpan(tracer, "markup.admin.guardrails", h)
		})
	}
	if mw.handler != nil {
		mux.Handle("/metrics", mw.handler)
	}
	mux.Handle("/healthz", httpapi.Healthz())
	mux.Handle("/readyz", httpapi.Readyz(isReady))
	markReady()
	return httpapi.WithCorrelationID(httpapi.WithTraceContext(httpapi.WithAccessLog(log, mux))), result, nil
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

// buildGuardrailRules is the flag-side adapter onto guardrails.BuildRules.
// fs.Visit detects which guardrail flags were explicitly set on the
// command line so an unset flag becomes a nil axis in the RuleSpec
// (no rule of that type), not a zero-value rule. Errors from BuildRules
// are reformatted to name the --flag the operator passed so boot-fail
// stderr remains actionable.
//
// Returns an empty slice when no guardrail flag was set -- the wire
// functions then mount no guardrails decorator and the binary serves
// with zero overhead.
func buildGuardrailRules(fs *flag.FlagSet, minF, maxF float64, countriesCSV, requiredCSV string) ([]guardrails.Rule, error) {
	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	var spec guardrails.RuleSpec
	if setFlags["min-factor"] || setFlags["max-factor"] {
		spec.Factor = &guardrails.FactorSpec{Min: minF, Max: maxF}
	}
	if setFlags["allowed-countries"] {
		spec.AllowedCountries = guardrails.SplitCSV(countriesCSV)
		if spec.AllowedCountries == nil {
			spec.AllowedCountries = []string{}
		}
	}
	if setFlags["required-fields"] {
		spec.RequiredFields = guardrails.SplitCSV(requiredCSV)
		if spec.RequiredFields == nil {
			spec.RequiredFields = []string{}
		}
	}

	rules, err := guardrails.BuildRules(spec)
	if err == nil {
		return rules, nil
	}
	switch {
	case errors.Is(err, guardrails.ErrFactorRangeInverted):
		return nil, fmt.Errorf("--min-factor (%g) must not exceed --max-factor (%g)", minF, maxF)
	case errors.Is(err, guardrails.ErrAllowedCountriesEmpty):
		return nil, fmt.Errorf("--allowed-countries was set with no values")
	case errors.Is(err, guardrails.ErrRequiredFieldsEmpty):
		return nil, fmt.Errorf("--required-fields was set with no values")
	default:
		return nil, err
	}
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

// bodyLoader implements httpapi.ReloadBodyLoader for the cmd-side
// integration. Supports recognizes text/csv (parsed via load.FromCSV,
// Diagnose'd, then built with the boot-time adapter) and
// application/json (parsed via snapshot.Read, loaded via
// LoadIntoIndexedDecider). See ADR-0030.
type bodyLoader struct {
	adapter      string
	modelVersion string
	stderr       io.Writer
}

func newBodyLoader(adapter, modelVersion string, stderr io.Writer) *bodyLoader {
	return &bodyLoader{adapter: adapter, modelVersion: modelVersion, stderr: stderr}
}

func (b *bodyLoader) Supports(mediaType string) bool {
	return mediaType == "text/csv" || mediaType == "application/json"
}

func (b *bodyLoader) Load(mediaType string, body []byte) (markup.Decider, httpapi.ReloadResult, error) {
	switch mediaType {
	case "text/csv":
		rules, err := load.FromCSV(bytes.NewReader(body))
		if err != nil {
			fmt.Fprintf(b.stderr, "body loader: parse csv body: %v\n", err)
			return nil, httpapi.ReloadResult{}, &markup.InvalidRuleSetError{Path: "<body>", Err: err}
		}
		diag := load.Diagnose(rules)
		if !diag.IsHealthy() {
			return nil, httpapi.ReloadResult{}, &httpapi.DiagnoseRejectedError{Diagnosis: diag}
		}
		decider, err := buildDecider(b.adapter, rules, b.modelVersion)
		if err != nil {
			fmt.Fprintf(b.stderr, "body loader: build decider: %v\n", err)
			return nil, httpapi.ReloadResult{}, fmt.Errorf("build decider: %w", err)
		}
		return decider, httpapi.ReloadResult{RuleCount: len(rules), ModelVersion: b.modelVersion}, nil
	case "application/json":
		snap, err := snapshot.Read(bytes.NewReader(body))
		if err != nil {
			fmt.Fprintf(b.stderr, "body loader: read snapshot body: %v\n", err)
			return nil, httpapi.ReloadResult{}, &markup.InvalidRuleSetError{Path: "<body>", Err: err}
		}
		decider, err := snapshot.LoadIntoIndexedDecider(snap)
		if err != nil {
			fmt.Fprintf(b.stderr, "body loader: load snapshot: %v\n", err)
			return nil, httpapi.ReloadResult{}, fmt.Errorf("load snapshot: %w", err)
		}
		return decider, httpapi.ReloadResult{RuleCount: len(snap.EngineSnapshot.Rules), ModelVersion: snap.ModelVersion}, nil
	default:
		return nil, httpapi.ReloadResult{}, fmt.Errorf("unsupported media type %q", mediaType)
	}
}
