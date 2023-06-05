# 30. Body-based `/admin/reload`

## Status

Accepted — `POST /admin/reload` accepts an optional non-empty request body. When the body is non-empty AND `Content-Type` matches a recognized bre-go format (`text/csv` or `application/json`) AND the server was constructed with `httpapi.WithReloadBodyLoader(fn)`, the handler dispatches the body to the configured loader closure (provided by cmd, which knows the boot-time adapter and model version). The closure parses with bre-go's existing parsers, runs Diagnose against the parsed rules, and returns a freshly-built Decider that the handler swaps into the active holder. Empty bodies, unrecognized Content-Types, or absent body-loader configuration all fall through to the existing file-on-disk path — bit-for-bit identical to v0.1.18 behavior. No new external dependency; reuses `internal/load.FromCSV`, `internal/load.Diagnose`, `internal/snapshot.Read`, and `internal/snapshot.LoadIntoIndexedDecider`.

## Context

A separate control plane operates `/admin/reload` programmatically across many running markup-svc instances. Today's reload contract is "re-read the file the server was told to read at boot" — the POST body is ignored; the source of truth lives on disk. For control-plane workflows this couples the deploy mechanism to whoever can write the on-disk file: a Kubernetes ConfigMap mutation (needs cluster RBAC, has a kubelet sync window of ~30 s, doesn't apply outside K8s) or a bind-mount edit (doesn't generalize past compose-local). The friction is real and the control plane has no clean general-purpose push mechanism.

Body-based reload removes that coupling: the control plane (or any operator with HTTP access) POSTs the new rules in the request body, markup-svc parses + Diagnose-gates + swaps in-process. No K8s permissions. No sync race. Works the same across compose, Kubernetes, bare metal.

The change is small in surface area and large in operational consequence. The hard constraint is **zero observable change** for existing operators on the file-based path:

- The existing e2e tests use empty-body POSTs against file loaders. They must pass unmodified.
- Operators who today curl `/admin/reload` with random bodies (the empty-data-binary case, weird Content-Types from defaults) must not suddenly get errors. Their bodies must continue to be ignored, as they are today.
- The file-based path's Diagnose gate (ADR-0026), `InvalidRuleSetError` 400 contract (ADR-0027), 405-on-non-POST (RFC 7231), and 500-on-loader-error semantics must be untouched.

The dispatch rule is therefore conservative: invoke the body loader ONLY when (a) body is non-empty, (b) Content-Type is a recognized bre-go format string, AND (c) a body-loader is wired by cmd. Failing any of those three falls through to today's file path. The bit-for-bit canary is the existing test suite.

### Three design choices

**1. Body-based path is opt-in via cmd configuration, not via flag.** Cmd that wants to support body-based reload passes `httpapi.WithReloadBodyLoader(closure)` to `httpapi.Reload(...)`. Cmd that doesn't configure a body-loader gets today's behavior — operators don't need any flag, no behavior changes on existing deployments. New operators who want body-based reload run the bumped binary; cmd wires the body-loader unconditionally so the contract is "supported whenever you have the binary." This avoids a `--body-reload-enabled` flag whose default would either break operators (default-on) or hide the capability (default-off).

**2. Content-Type matching is the dispatch trigger, not body-non-empty.** The handler checks Content-Type against a small whitelist of recognized format strings before invoking the body-loader. Random non-empty bodies (operator curls with `application/x-www-form-urlencoded` from a default, garbage payload tests) fall through to the file-based path — exactly today's behavior. Operators who curl-with-default cannot accidentally trigger body-based parsing.

**3. Body-loader closure knows about the boot-time adapter and model.** The handler's body-loader signature is `func(contentType string, body []byte) (markup.Decider, ReloadResult, error)` — format-only. The cmd-side closure captures `--adapter`, `--model`, and stderr at construction time. When parsing a CSV body, it uses the boot-time `--adapter` to build the Decider. This matches operator expectations: a server booted with `--adapter=indexed` continues to serve via the indexed adapter when body-based-reloaded; the body just carries new rules, not a new adapter choice.

Snapshots are inherently indexed-adapter bound (per ADR-0007). A `application/json` body POST against a server booted with `--rules` (CSV mode) is accepted only when the snapshot's internal shape matches what `snapshot.LoadIntoIndexedDecider` expects — `snapshot.Read` returns `ErrFormatVersionMismatch` or a malformed-condition error otherwise, surfacing as `InvalidRuleSetError` → 400.

## Decision

### New types in `internal/httpapi/reload.go`

```go
// ReloadBodyLoader handles body-based reload. The interface owns its
// supported-Content-Type list (Supports) so the dispatch decision lives
// in one place rather than being duplicated in the handler. Load parses
// the body, runs format-appropriate validation (Diagnose for CSV;
// snapshot schema check for JSON), and builds the freshly-loaded
// Decider. Cmd's implementation captures boot-time --adapter and
// --model so the interface stays format-only.
type ReloadBodyLoader interface {
    Supports(contentType string) bool
    Load(contentType string, body []byte) (markup.Decider, ReloadResult, error)
}

// DiagnoseRejectedError wraps a Diagnose result returned from a
// ReloadBodyLoader.Load call. The handler unwraps and passes the
// embedded Diagnosis to the existing writeDiagnoseRejection helper —
// the wire shape is identical to the file-path Diagnose rejection
// (ADR-0026); the typed error is only the data carrier from loader
// to handler.
type DiagnoseRejectedError struct {
    Diagnosis markup.Diagnosis
}

func (e *DiagnoseRejectedError) Error() string {
    return "reload rejected: rule set failed Diagnose"
}

// WithReloadBodyLoader enables the body-based reload path. When loader
// is non-nil, the request body is non-empty, AND loader.Supports(ct)
// returns true for the request's Content-Type, the handler dispatches
// to loader.Load. Absent the option, on empty body, or on
// loader.Supports(ct) == false, the handler falls through to the
// existing file-based path. See ADR-0030.
func WithReloadBodyLoader(loader ReloadBodyLoader) ReloadOption
```

The interface (rather than a function) lets the loader own its supported-format list. Adding a future format means extending one place — the cmd-side implementation's `Supports` switch — without modifying the httpapi handler's dispatch code (OCP-friendly).

### Handler dispatch logic

```
1. POST method check (unchanged: 405 on non-POST with Allow: POST)
2. Read request body (capped at 16 MB; cap chosen as ~8x the expected
   2 MB upper bound for a 100k-rule CSV, leaving headroom for snapshot
   bodies which are larger than CSV due to JSON overhead).
3. Parse Content-Type via mime.ParseMediaType so charset parameters,
   case differences, and whitespace are normalized by the stdlib.
4. If body is non-empty AND cfg.bodyLoader is set AND
   cfg.bodyLoader.Supports(mediaType) returns true:
     a. Call cfg.bodyLoader.Load(mediaType, body)
     b. On *DiagnoseRejectedError: handler extracts the Diagnosis and
        calls the existing writeDiagnoseRejection(w, d) helper — wire
        shape identical to the file-path rejection (ADR-0026).
     c. On *InvalidRuleSetError: 400 with the error string (per ADR-0027).
     d. On any other error: 500 with opaque "reload failed" body.
     e. On success: holder.Swap(decider); 200 with ReloadResult JSON.
5. Else (empty body OR Supports(mediaType) == false OR no body-loader):
   - The existing file-based path runs UNCHANGED: optional Diagnose gate,
     then fileLoader(), then swap. Status codes and response shape match
     v0.1.18 exactly. The existing e2e tests prove this.
```

The "empty body OR Supports == false" fall-through is the bit-for-bit-compat guarantee. An operator sending an empty-body POST against a binary that wires a body-loader still hits the file path. An operator curling with `--data 'foo'` (which carries `Content-Type: application/x-www-form-urlencoded` by default) hits the file path because the body-loader's `Supports` returns false for that media type. A curl with explicit `Content-Type: text/csv` and a non-empty body is the only thing that triggers body-based parsing — and that is the intended use.

### Recognized Content-Type set

The cmd-side ReloadBodyLoader's `Supports` returns true for these media types (after `mime.ParseMediaType` normalization):

- `text/csv` → `Load` parses via `internal/load.FromCSV`; runs `internal/load.Diagnose` against the parsed rules; on Diagnose-unhealthy returns `*DiagnoseRejectedError{Diagnosis: d}`; on parse failure returns `*markup.InvalidRuleSetError`; on healthy builds via the boot-time adapter choice.
- `application/json` → `Load` parses via `internal/snapshot.Read` and calls `internal/snapshot.LoadIntoIndexedDecider`. Diagnose is NOT run on this path. Snapshots are validated at build time per ADR-0007 (cmd/snapshot-build runs Diagnose before writing); `snapshot.Read` validates the schema (FormatVersion, embedded engine snapshot, factor map presence per ADR-0007's `ErrMissingFactor`). The deliberate Diagnose-skip is acknowledged as a security delta in Not Closed below.

### cmd wiring (`cmd/markup-server/main.go`)

Cmd defines a `bodyLoader` struct implementing `ReloadBodyLoader`. The struct holds the boot-time `--adapter`, `--model`, and stderr writer as fields. `Supports` returns true for `text/csv` and `application/json`. `Load` dispatches by media type to the appropriate bre-go parser. On parse failure, wraps with `&markup.InvalidRuleSetError{Path: "<body>", Err: err}`. On Diagnose-unhealthy (CSV path only), returns `&httpapi.DiagnoseRejectedError{Diagnosis: diag}`. The named struct is unit-testable directly — easier than driving an anonymous closure through `run(...)`.

The existing file-based `fileLoader` and `diagnoseFn` continue to be passed to `httpapi.Reload(...)` unchanged. The handler dispatches between body and file paths based on the runtime conditions above.

Body-loader is wired unconditionally — every binary built from this tag supports body-based reload when the request shape matches. There is no `--body-reload-enabled` flag; the contract is "if the binary supports it, sending a recognized Content-Type with a non-empty body triggers it."

### Tests

`internal/httpapi/reload_body_test.go` (new):

- `TestReload_EmptyBody_FileBasedPath_Unchanged` — empty body POST goes through the file loader; existing canary.
- `TestReload_BodyLoaderWired_EmptyBody_StillHitsFilePath` — body-loader spy installed via `WithReloadBodyLoader`, empty-body POST sent; assert spy was NOT called and the file-loader path executed. This is the bit-for-bit-compat canary that makes the runtime branch (body length > 0) observable in tests, not just claimed.
- `TestReload_EmptyBody_RecognizedContentType_StillHitsFilePath` — body-loader installed, empty body sent with `Content-Type: text/csv`; assert body-loader NOT called (empty body short-circuits).
- `TestReload_TextCSV_HappyPath` — POST CSV body with `Content-Type: text/csv`; assert 200 + correct ReloadResult; assert holder swapped to new decider.
- `TestReload_TextCSV_WithCharset` — `Content-Type: text/csv; charset=utf-8` is normalized by `mime.ParseMediaType` to `text/csv`; same as happy path.
- `TestReload_TextCSV_InvalidRuleSet` — malformed CSV body; assert 400 + body identifies as InvalidRuleSetError-shaped per ADR-0027.
- `TestReload_TextCSV_DiagnoseRejected` — parseable CSV with duplicate rule names; assert 400 + issue list in response body (assert the wire shape matches the file-path Diagnose rejection from ADR-0026 — same `writeDiagnoseRejection` helper produces both).
- `TestReload_ApplicationJSON_HappyPath` — POST snapshot JSON body; assert 200 + ModelVersion from snapshot.
- `TestReload_ApplicationJSON_FormatVersionMismatch` — snapshot with wrong FormatVersion; assert 400 (InvalidRuleSetError shape).
- `TestReload_UnrecognizedContentType_FallsThrough` — POST with `Content-Type: text/plain` and body; assert file-loader was called instead of body-loader (today's behavior).
- `TestReload_NoBodyLoader_FallsThrough` — non-empty body but `Reload` constructed without `WithReloadBodyLoader`; assert file-loader was called.
- `TestReload_NoContentType_FallsThrough` — non-empty body with no Content-Type header; assert file-loader was called.
- `TestReload_BodyExceedsCap_Returns413` — POST with body larger than the 16 MB cap; assert 413 Payload Too Large.
- `TestReload_NonPOST_Returns405` — existing canary still passes.

cmd-level integration tests:

- `TestE2EBootsWithBodyLoader_EmptyBodyReloadUnchanged` — boot via `run(...)`, hit `/admin/reload` empty-body, assert response shape matches the v0.1.18 golden. Makes the cmd-level canary observable.
- `TestE2EBodyBasedReload_CSVHappyPath` — boot via `run(...)`, hit `/admin/reload` with a CSV body, then hit `/decide` and assert the new factor serves.

The existing test suite (`reload_test.go`, `reload_diagnose_test.go`) continues to use empty-body POSTs and asserts on the file-based path. None of them is modified. The new test file is additive.

## Consequences

### Closed

- A separate control plane (or any HTTP client) can push rule sets to running markup-svc instances without filesystem-level coordination. ConfigMap RBAC is no longer required for programmatic deploy.
- The bre-go canonical formats remain the contract. No new parser layer in markup-svc; the existing `load.FromCSV` and `snapshot.Read` paths handle both file-based and body-based reloads identically once parsed.
- Existing operators see zero observable change. Empty-body POSTs, unrecognized Content-Types, and weird-body curls all behave exactly as v0.1.18. The bit-for-bit-compat contract is the existing e2e test suite continuing to pass unmodified.
- Diagnose gating (ADR-0026) applies on the body path identically to the file path. Bad bodies do not swap; previous live rules continue serving.
- `InvalidRuleSetError` 400 contract (ADR-0027) applies on the body path. Parser failures return 400, not 500.

### Not closed

- **Authentication.** The body-based path is no more authenticated than the file-based path. Both rely on network-level controls (admin endpoint exposed only to trusted callers). Authn / authz is a separate ADR.
- **JSON snapshot path skips Diagnose — security delta versus CSV.** On the `text/csv` path, the handler runs `load.Diagnose` against the parsed rules and rejects unhealthy sets with 400. On the `application/json` snapshot path, no separate Diagnose pass runs — `snapshot.Read` validates schema/version, `LoadIntoIndexedDecider` validates per-rule factor presence, but neither catches semantic issues like out-of-range factors or no-op rules that Diagnose would. This is consistent with today's file-based snapshot path (also skips Diagnose, also at the same trust boundary), but body-based pushing widens the attack surface: a tampered snapshot now arrives over HTTP rather than through filesystem RBAC. Mitigated operationally by the same network-level controls protecting the admin endpoint, and architecturally by treating the snapshot's build-time Diagnose pass (cmd/snapshot-build + ADR-0007) as the trust source. Closing this gap by Diagnose-rebuilding the rule set from the snapshot's factor map is a viable approach but requires its own ADR.
- **Audit metadata in the response.** The 200 response carries `rule_count` and `model_version`. It does NOT carry a body-source hash or the operator identity. A separate ADR can extend `ReloadResult` to include audit fields.
- **Streaming bodies.** The handler reads the body fully into memory (capped at 16 MB). For artifacts beyond that, a streaming variant would need a separate code path. Out of scope; the cap covers expected rule-set sizes including 100k-rule CSVs.

### Performance impact

This ADR does NOT commit absolute performance numbers. Per the ADR-0012 protocol, performance bars are pre-registered in `scientific/v0.1.19/REPORT.md` BEFORE measurement, and that pre-registration ships in the v0.1.19 release commit alongside the benchmark code itself. The qualitative shape is described here; the absolute numbers are the harness's job.

- **Empty-body path: sub-microsecond dispatch overhead.** The handler adds a body-length check, a Content-Type header read via `mime.ParseMediaType`, a `Supports` call on the body-loader (nil-check + map/switch lookup), and on empty-body falls into the existing file-based code. The added work is sub-microsecond per request and irrelevant at the sub-1-QPS profile admin endpoints actually see. The bit-for-bit-compat canary is the existing e2e test suite continuing to pass unmodified.

- **CSV body path:** work is dominated by `load.FromCSV` (linear in rule count), `load.Diagnose` (linear in rule count per markup-svc/ADR-0025's O(N) characterization), and indexed-adapter index build (linear-ish in rule count). At small N (100s of rules) the total is in the small-microseconds to low-milliseconds range; at large N (100k rules) the total grows into seconds. The control-plane workflow's reason for pre-compiling snapshots is exactly this scaling: at 100k rules the CSV path's parse + Diagnose + index build dominates wall time.

- **Snapshot body path (application/json):** work is `snapshot.Read` JSON decode + `LoadIntoIndexedDecider` factor-reattachment. Both are well-defined linear-in-snapshot-size operations with no parser-vs-index rebuild step. This is the fast path the snapshot format exists for; the operational expectation is that snapshot body reload is materially faster than CSV body reload for large rule sets, but the exact crossover is a measurement question for the v0.1.19 harness, not a claim for this ADR.

- **Memory.** Body buffer is held for the duration of parse + build. At 16 MB cap and the typical 0–1 concurrent admin POSTs (operator-trigger frequency), peak transient working set is bounded by `body_size + parsed_rules + decider_index` — on the order of low-tens of MB for the 100k-rule case, well within typical markup-svc container budgets.

### Scientific harness pre-registration

`scientific/v0.1.19/` lands in the v0.1.19 release commit with:

- `BenchmarkReload_EmptyBody` — measures the file-based path end-to-end (existing handler behavior). Two-trial pre-registration: measure on the v0.1.18 tag to establish the baseline, measure on v0.1.19 with the body-loader wired but empty body sent. Bar: v0.1.19 measurement matches v0.1.18 baseline within 2σ. This is the zero-overhead-on-empty-body canary made measurable.
- `BenchmarkReload_CSVBody_100Rules` — pilot-derived absolute bar from a v0.1.19 pilot run committed in the pre-registration. Sets a defensible mean + 2σ ceiling rather than a feel-based number.
- `BenchmarkReload_CSVBody_100kRules` — pilot-derived absolute bar.
- `BenchmarkReload_SnapshotBody_100kRules` — pilot-derived absolute bar.

The tag cut blocks on all four bars passing under measurement. None of the bars in this ADR carry specific microsecond or millisecond claims — the v0.1.19 pre-registration commit owns those numbers, derived from pilot measurements per the ADR-0012 protocol.
