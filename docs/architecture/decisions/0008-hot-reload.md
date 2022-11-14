# 8. Hot Reload via Admin Endpoint

## Status

Proposed — proposes `POST /admin/reload` backed by a thin atomic holder around `markup.Decider`. The endpoint reads the latest CSV (or snapshot) from disk, builds a fresh Decider, and atomically replaces the active Decider so subsequent `/decide` calls see the new rule set without a process restart.

## Context

Rule sets change. Today the markup-server holds one Decider constructed at boot from `--rules` or `--snapshot`; updating the rule set requires a process restart, which pays the cold-start cost again (parse + bucket build for indexed, or LoadSnapshot for the snapshot path) and incurs brief downtime as in-flight requests drain through SIGTERM.

Restarts work for occasional updates. They become painful for:

- **Hotfixes** — an urgent rule change that should not wait for a deploy window.
- **A/B rollouts** — frequent model-version transitions during experimentation.
- **Snapshot promotion** — a build job emits a fresh `snapshot.json` (per ADR-0007); the running server should be able to adopt it without a restart.

Hot reload swaps the active Decider in-place. The HTTP transport already serves `/decide`; an admin route on the same server is the simplest delivery mechanism.

Three design questions.

### 1. Trigger mechanism

Candidates:

- **HTTP admin endpoint** (`POST /admin/reload`)
- **SIGHUP signal** handler
- **File-watcher** polling the source path

HTTP first because:

1. Easiest to drive from CI/CD pipelines (`curl -X POST .../admin/reload`).
2. Composes with auth middleware when ADR-0003's auth follow-up lands — same transport, same middleware chain.
3. No new server, no new lifecycle to test. The existing `http.Server` already mounts `/decide` and `WithCorrelationID`; the admin endpoint slots in beside them.
4. Programmable response — returns the new rule count and `ModelVersion` so the operator (or pipeline) sees what is now serving.

SIGHUP is fine for unix-y deployments but harder to drive from container orchestrators that own PID 1. File-watcher adds an external dependency (`fsnotify`) and an inotify/polling failure mode; not worth the complexity at this scope.

### 2. Atomicity guarantee

The active Decider is held behind a thin holder type that synchronizes reads and writes:

- Every `Decide` call acquires a read lock for the duration of the call.
- The swap acquires a write lock just long enough to replace the inner Decider pointer.

With `sync.RWMutex`:

- A `Decide` call already past the `RLock` sees its captured Decider through to completion. Held the way Go's RWMutex works, the call cannot be interrupted by a writer — the swap waits.
- A `Decide` call arriving during the swap waits for the `Lock` to release, then sees the new Decider.
- The old Decider is garbage-collected once no `Decide` call retains a reference.

Per-`Decide` overhead is one `RLock` + `RUnlock`. Uncontended RWMutex RLock is `sync/atomic`-backed: tens of nanoseconds on commodity hardware. Against the engine's per-rule `Eval` cost (microseconds at typical rule-set sizes), invisible.

**Alternative considered**: `atomic.Pointer[markup.Decider]`. Not available at the project's Go 1.18 baseline. `atomic.Value` works in 1.18 but its `Store` panics if the concrete stored type changes between calls — using it would require wrapping every Decider in a stable struct type before storing. The RWMutex approach is type-safe, well-understood, and the overhead is negligible at any practical QPS.

**Honest framing**: the lock is not lock-free. Under sustained read load with rare swaps, the RWMutex degrades to nearly free (RWMutex's read path is uncontended when no writer waits). If a future profile shows real contention, the atomic.Value-with-wrapper approach is the migration target and the holder package is the single change site.

### 3. What can be reloaded

For v0.0.3, conservative scope:

- The reload reads from the **same source type** used at boot. If the server was booted with `--rules`, reload re-reads that CSV path. If booted with `--snapshot`, reload re-reads that snapshot path.
- The **same adapter** stays active for the process's lifetime. Boot picks the adapter via `--adapter` (or implicitly `indexed` for the snapshot path); reload reuses that choice.
- The **same source path**. Path changes require a process restart.

Cross-type reloads (rules→snapshot mid-flight) introduce edge cases — what does `--adapter` mean if it was ignored at boot but a CSV path now appears? — and the answers are operationally interesting but out of scope here. Cross-adapter reloads have the additional problem that not all rules accept every adapter (per ADR-0006 the indexed adapter rejects non-indexable conditions); validating that mid-flight requires more design than this ADR commits to.

## Decision

`internal/decider/swap` ships:

```go
// Decider holds a markup.Decider behind a RWMutex and satisfies the
// markup.Decider port itself. Decide acquires the read lock for the
// duration of the call; Swap acquires the write lock just long enough
// to replace the inner pointer.
type Decider struct {
    mu    sync.RWMutex
    inner markup.Decider
}

func New(initial markup.Decider) *Decider
func (d *Decider) Decide(ctx context.Context, req markup.Request) (markup.Decision, error)
func (d *Decider) Swap(next markup.Decider)
```

`internal/httpapi` ships:

```go
// Reload returns an http.Handler that, on POST, calls loader() to build
// a fresh Decider, atomically swaps it into holder via holder.Swap, and
// responds with 200 + {rule_count, model_version}. Non-POST returns 405
// with Allow: POST. loader errors map per ADR-0003's error table.
type ReloadResult struct {
    RuleCount    int
    ModelVersion string
}
type Loader func() (markup.Decider, ReloadResult, error)

func Reload(holder *swap.Decider, loader Loader) http.Handler
```

`cmd/markup-server` wires:

```go
holder := swap.New(initialDecider)
mux.Handle("/decide", httpapi.Decide(holder))
mux.Handle("/admin/reload", httpapi.Reload(holder, loaderClosure))
```

The `loaderClosure` captures the source path, model version, and adapter selected at boot, so reload re-runs the original boot path against the current disk contents.

## Consequences

### Closed by this ADR

- Hot reload works for both the `--rules` and `--snapshot` boot paths.
- `/admin/reload` is part of the same HTTP server as `/decide` — one process, one listener, one middleware chain.
- The active Decider is type-safe and lock-coordinated; in-flight `Decide` calls finish on their captured Decider, new `Decide` calls land on the swapped Decider.
- The reload response includes rule count + `ModelVersion` so callers see what is now serving.

### NOT closed by this ADR

- Authentication on `/admin/reload`. The endpoint is currently unprotected, matching ADR-0003's "trusted boundary" posture. Auth is its own ADR.
- Rate limiting on `/admin/reload`. Same boundary argument.
- Cross-source-type reloads (rules→snapshot or vice versa). Out of scope.
- Cross-adapter reloads. Out of scope.
- File-watcher / SIGHUP trigger mechanisms. Out of scope.
- Optimistic concurrency control (compare-and-swap on a version token). Out of scope; sequential reloads are linearized through the write lock.

### Performance impact

Per-`Decide` overhead from the holder:

- One `RLock` + `RUnlock` per call. Uncontended path is `sync/atomic`-backed and measures in the tens of nanoseconds.
- One method call indirection (`holder.Decide` → `inner.Decide`). The holder's `Decide` is a non-virtual call into the concrete `*swap.Decider`; the `inner.Decide` is an interface dispatch (one indirection that already exists at every `markup.Decider` consumer site, not new cost).

Aggregate: per-`Decide` overhead measured in tens of nanoseconds against the engine's microsecond-scale `parser.Condition.Eval` walk. Invisible at any practical QPS.

Reload cost equals the original boot path: parse + AddRule + Build for indexed (or LoadSnapshot + Build for the snapshot path). The swap itself is one pointer assignment under the Lock — sub-microsecond. During the swap, new `Decide` calls block on `RLock` for the duration of the Lock — measured at the same tens-of-nanoseconds scale unless the swap holds the Lock for unusually long (it does not).

### Validation strategy

- `internal/decider/swap`: unit tests for the holder. A `TestSwapUnderConcurrentDecide` runs the race detector with many goroutines repeatedly calling `Decide` while another goroutine `Swap`s repeatedly; both paths complete without races and without lost writes. A `TestPreSwapAndPostSwapDecisions` confirms a `Decide` issued before `Swap` returns sees the old Decider's `Rule`, and a `Decide` issued after `Swap` returns sees the new one.
- `internal/httpapi`: handler tests for `Reload`. Happy path (POST returns 200 + new `RuleCount` + `ModelVersion` JSON), method not allowed (GET returns 405 + `Allow: POST`), loader error → 5xx with opaque body (the operator sees the error in logs, not in the response). The non-POST guard mirrors ADR-0003's pattern for `/decide`.
- `cmd/markup-server`: end-to-end test that boots with `--rules`, hits `/decide` and asserts the original factor, overwrites the CSV with a new factor on disk, `POST`s `/admin/reload`, then hits `/decide` and asserts the new factor. This is the load-bearing test: a successful reload must produce an observably-changed Decision, not just a 200 response.
