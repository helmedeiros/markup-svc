package harness

import (
	"bytes"
	"context"
	"os"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/helmedeiros/markup-svc/internal/decider/firstmatch"
	"github.com/helmedeiros/markup-svc/internal/decider/indexed"
	"github.com/helmedeiros/markup-svc/internal/decider/inmemory"
	"github.com/helmedeiros/markup-svc/internal/decider/priority"
	"github.com/helmedeiros/markup-svc/internal/decider/router"
	"github.com/helmedeiros/markup-svc/internal/decider/swap"
	"github.com/helmedeiros/markup-svc/internal/load"
	"github.com/helmedeiros/markup-svc/internal/markup"
	"github.com/helmedeiros/markup-svc/internal/observability/metrics"
	mkotel "github.com/helmedeiros/markup-svc/internal/observability/otel"
	"github.com/helmedeiros/markup-svc/internal/snapshot"
)

// benchRequest is the Request every Decide-path benchmark uses.
// Matches exactly one rule in the fixture (de_t5: country=DE,
// customer_tier=t5) — the realistic operational shape per ADR-0012.
var benchRequest = markup.Request{Country: "DE", CustomerTier: "t5"}

// benchCtx is the background context every Decide-path benchmark
// uses. The router policy + correlation-ID-based attributes are
// exercised by their dedicated package benchmarks; the harness
// measures the steady-state Decide path with no correlation ID.
var benchCtx = context.Background()

// BenchmarkAdapter measures per-Decide latency for each of the four
// shipped adapter packages against the same fixture and Request.
// Adapters are run as sub-benchmarks of a single parent so a single
// `go test -bench=BenchmarkAdapter` pass with `-count=50` interleaves
// the four adapters per pass — host noise hits every measurement
// equally rather than punishing whichever runs last.
func BenchmarkAdapter(b *testing.B) {
	rules := loadFixture(b)

	b.Run("inmemory", func(b *testing.B) {
		d, err := inmemory.NewFromRules(rules, "bench")
		if err != nil {
			b.Fatalf("inmemory.NewFromRules: %v", err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = d.Decide(benchCtx, benchRequest)
		}
	})

	b.Run("firstmatch", func(b *testing.B) {
		d, err := firstmatch.NewFromRules(rules, "bench")
		if err != nil {
			b.Fatalf("firstmatch.NewFromRules: %v", err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = d.Decide(benchCtx, benchRequest)
		}
	})

	b.Run("priority", func(b *testing.B) {
		d, err := priority.NewFromRules(rules, "bench")
		if err != nil {
			b.Fatalf("priority.NewFromRules: %v", err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = d.Decide(benchCtx, benchRequest)
		}
	})

	b.Run("indexed", func(b *testing.B) {
		d, err := indexed.NewFromRules(rules, "bench")
		if err != nil {
			b.Fatalf("indexed.NewFromRules: %v", err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = d.Decide(benchCtx, benchRequest)
		}
	})
}

// BenchmarkDecorator measures the per-Decide overhead of each
// shipped decorator on top of the indexed adapter (the baseline most
// production deployments use). Stacking is per the project's
// recommended composition: metrics → otel → swap → engine.
func BenchmarkDecorator(b *testing.B) {
	rules := loadFixture(b)
	baseline, err := indexed.NewFromRules(rules, "bench")
	if err != nil {
		b.Fatalf("indexed.NewFromRules: %v", err)
	}

	b.Run("swap", func(b *testing.B) {
		d := swap.New(baseline)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = d.Decide(benchCtx, benchRequest)
		}
	})

	b.Run("otel", func(b *testing.B) {
		tp := trace.NewNoopTracerProvider()
		d := mkotel.Wrap(baseline, tp.Tracer("bench"))
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = d.Decide(benchCtx, benchRequest)
		}
	})

	b.Run("metrics", func(b *testing.B) {
		sink := &metrics.RecordingSink{}
		d := metrics.Wrap(baseline, sink)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = d.Decide(benchCtx, benchRequest)
		}
	})

	b.Run("full-stack", func(b *testing.B) {
		tp := trace.NewNoopTracerProvider()
		sink := &metrics.RecordingSink{}
		d := metrics.Wrap(mkotel.Wrap(swap.New(baseline), tp.Tracer("bench")), sink)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = d.Decide(benchCtx, benchRequest)
		}
	})
}

// BenchmarkColdStart measures the time to load a fresh Decider from
// the two on-disk formats: a raw CSV (parse + bucket + Build) vs a
// pre-built snapshot (read JSON + LoadSnapshot). The bar this
// measurement supports is ADR-0007's claim that snapshot loading
// skips parser.ParseToCondition.
func BenchmarkColdStart(b *testing.B) {
	b.Run("rules", func(b *testing.B) {
		path := fixturePath(b)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			f, err := os.Open(path)
			if err != nil {
				b.Fatalf("open fixture: %v", err)
			}
			rules, err := load.FromCSV(f)
			_ = f.Close()
			if err != nil {
				b.Fatalf("load.FromCSV: %v", err)
			}
			_, err = indexed.NewFromRules(rules, "bench")
			if err != nil {
				b.Fatalf("indexed.NewFromRules: %v", err)
			}
		}
	})

	b.Run("snapshot", func(b *testing.B) {
		// Pre-build the snapshot bytes once; the snapshot path itself
		// is what we measure — reading those bytes back and
		// reconstituting the Decider, NOT writing them.
		rules := loadFixture(b)
		snap, err := snapshot.Build(rules, "bench")
		if err != nil {
			b.Fatalf("snapshot.Build: %v", err)
		}
		var buf bytes.Buffer
		if err := snapshot.Write(&buf, snap); err != nil {
			b.Fatalf("snapshot.Write: %v", err)
		}
		snapBytes := buf.Bytes()

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			loaded, err := snapshot.Read(bytes.NewReader(snapBytes))
			if err != nil {
				b.Fatalf("snapshot.Read: %v", err)
			}
			_, err = snapshot.LoadIntoIndexedDecider(loaded)
			if err != nil {
				b.Fatalf("snapshot.LoadIntoIndexedDecider: %v", err)
			}
		}
	})
}

// BenchmarkRouter measures the per-Decide overhead added by the
// router over a single-route deployment. Single route + DefaultPolicy
// is the no-dispatch worst case — the router runs its policy + stamp
// path but has nothing to choose between, so what the measurement
// captures is the pure decorator overhead.
func BenchmarkRouter(b *testing.B) {
	rules := loadFixture(b)
	baseline, err := indexed.NewFromRules(rules, "bench")
	if err != nil {
		b.Fatalf("indexed.NewFromRules: %v", err)
	}

	b.Run("single-route", func(b *testing.B) {
		routes := []router.Route{
			{ModelVersion: "bench", Variant: "control", Decider: baseline},
		}
		r := router.New(routes, router.DefaultPolicy{})
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = r.Decide(benchCtx, benchRequest)
		}
	})
}
