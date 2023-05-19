# 27. `InvalidRuleSetError` ↦ 400 on `/admin/reload` + `/admin/diagnose`

## Status

Accepted — `internal/markup.InvalidRuleSetError` is a typed error that wraps any rule-set parse / validation failure with `Path` + underlying `Err`. The httpapi handlers (`Reload`, `Diagnose`) detect it via `errors.As` and return `400 Bad Request` instead of `500 Internal Server Error`. `loadRulesFromFile` in `cmd/markup-server` wraps `load.FromCSV` failures with `InvalidRuleSetError`. File-open failures stay 500 (they are not unambiguously caller-side). Existing E2E test updated to assert the new contract.

## Context

pricing-observability/ADR-0014 (`AdminHotReloadRejected`) originally fired on `gateway_requests_total{method="POST",status="400"}` because the Diagnose-gated reload was expected to return 400 on rejection. During the v0.0.18 work I observed that markup-svc's `/admin/reload` returned 500, not 400, when the rules CSV failed at the parser stage (e.g., an unterminated quote, a syntactically invalid condition). pricing-observability v0.0.18 widened the alert expression to `status=~"[45].."` as a workaround so the coverage gap didn't keep the alert silent.

The widening worked but the underlying semantic is wrong: a parse failure on operator-supplied rules is a caller-side error. Posting bad config to `/admin/reload` is the same shape as posting malformed JSON to `/decide` — that already returns 400 (`internal/httpapi/decide.go:76`). Returning 500 for parse failure conflates "the operator gave me bad input" with "the server itself is broken." This matters for:

- **Alerting.** A real 500 on `/admin/reload` would indicate a server-side fault (file IO bug, panic in loader code). The current behavior (500 on parse error) drowns that signal in caller-side errors.
- **Client retries.** A 4xx tells the client not to retry; a 5xx invites retry. Operator tooling retrying a parse failure repeatedly is wasted load.
- **Documentation.** RFC 7231 §6.5.1: "The 400 (Bad Request) status code indicates that the server cannot or will not process the request due to something that is perceived to be a client error (e.g., malformed request syntax, invalid request message framing, or deceptive request routing)." Malformed rule CSV is exactly that.

Two design options.

### 1. Pattern-match on the error string

`if strings.HasPrefix(err.Error(), "parse rules ") { 400 } else { 500 }`. Cheap, no new types.

Pros: smallest diff.
Cons: brittle — any reword of the error message at line `loadRulesFromFile` breaks the contract silently. The CI test would catch it but a refactor in cmd would have to know to keep the prefix. Bad coupling.

### 2. Typed sentinel error wrapped at the load site

A `markup.InvalidRuleSetError` type with `Unwrap() error` so it composes with the existing error chain. cmd's loader wraps parser errors with it; the handler dispatches via `errors.As`.

Pros: type-system enforces the contract; refactors in cmd that change the error wording stay correct; future loaders (e.g., the snapshot path) opt in by wrapping their own parse errors the same way; the sentinel lives in `internal/markup` (the port) so neither httpapi nor cmd takes a dep on `internal/load`.
Cons: one new file in `internal/markup` (~25 lines); cmd has to wrap the error explicitly.

**Pick option 2.** Type-system enforcement is the standard pattern for this; the diff is small; the layering stays clean.

### File-open errors stay 500

A separate question: when `os.Open(rulesPath)` fails (file deleted, permissions changed), is that 400 or 500? Arguments both ways:

- **400**: the operator told the server to read a file that doesn't exist. Caller-side misconfiguration.
- **500**: from the HTTP client's perspective, the file the server reads is part of the server's environment. The operator submitting a reload request didn't *supply* the file in the POST body; they triggered the server to re-read it.

The conservative call is 500. The operator who renamed a mounted file mid-reload is operating outside the contract (compose / k8s mounts make this rare), and falling back to 500 keeps the path consistent with `os.Open` failures elsewhere in the code base. If a real workflow needs 400 here, a follow-up can split `*os.PathError` → 400.

## Decision

`internal/markup/errors.go`:

```go
type InvalidRuleSetError struct {
    Path string
    Err  error
}

func (e *InvalidRuleSetError) Error() string { /* ... */ }
func (e *InvalidRuleSetError) Unwrap() error { return e.Err }
```

`cmd/markup-server/main.go` `loadRulesFromFile`: wraps `load.FromCSV` failures with `&markup.InvalidRuleSetError{Path: path, Err: err}`. File-open failures stay as `fmt.Errorf("open rules %q: %w", path, err)` for 500 mapping.

`internal/httpapi/reload.go` + `diagnose.go`: dispatch via a shared `statusForLoadErr` helper:

```go
func statusForLoadErr(err error) int {
    var ire *markup.InvalidRuleSetError
    if errors.As(err, &ire) {
        return http.StatusBadRequest
    }
    return http.StatusInternalServerError
}
```

Called from both `Reload` (the diagnose-gate path AND the loader-stage path) and `Diagnose`.

Existing E2E `TestE2EReloadFailureKeepsOldDecider` updated to assert 400 with a reference to this ADR. Two new unit tests:

- `TestReloadInvalidRuleSetReturns400` — synthetic `InvalidRuleSetError` from the loader returns 400; the holder still serves the old decider.
- `TestDiagnose_InvalidRuleSetReturns400` — synthetic `InvalidRuleSetError` from the diagnose fn returns 400 on `GET /admin/diagnose`.

## Consequences

### Closed

- `/admin/reload` returns 400 when the rules CSV is malformed. Operators retrying a parse error see "stop retrying" semantics.
- `/admin/diagnose` returns 400 (was 500) when the underlying rules cannot be parsed. Same client-error semantics.
- pricing-observability can re-narrow `AdminHotReloadRejected` from `status=~"[45].."` to `status=~"4.."` in a follow-up commit. 5xx on `/admin/reload` now strictly indicates a server fault, which is a different alert class (could ship as `AdminReloadServerError` later).
- The semantics document themselves via the type: any future loader that returns an InvalidRuleSetError flows through the same 400 path with no handler changes.

### Not closed

- File-open errors. `os.Open` failures stay 500 (see Context above). A workflow that needs 400 here can extend `statusForLoadErr` to check for `*fs.PathError` with a follow-up ADR.
- Snapshot-loader parse errors. `snapshotLoader` returns raw errors from `snapshot.Read` / `snapshot.LoadIntoIndexedDecider`. The same wrapping pattern can apply when the snapshot path actually surfaces operator-supplied snapshots; today the snapshot bundle is internally produced, so its parse errors really are server-side and 500 is correct.
- Body-payload reload. Today `/admin/reload` re-reads the on-disk file regardless of POST body. A future ADR introducing body-supplied reload (operator POSTs the new CSV in the request body) would route ALL parse errors through this same 400 path automatically — the type already covers it.

### Performance impact

- One pointer dereference + type assertion per `/admin/reload` and `/admin/diagnose` call. Both endpoints are operator-triggered, not on the hot path. Negligible.
- Compile-time: zero (no new dependency between packages — `markup` is already imported by `httpapi`).
