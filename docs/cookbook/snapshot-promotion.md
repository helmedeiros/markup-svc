# Promote a rule set via snapshot artifact

## Problem

Your rule set is large enough that startup parsing has become a noticeable share of deploy time. You want to bake the parsed/indexed form into a CI artifact and ship that to production instead of re-parsing the raw CSV on every replica restart.

## Recipe

In CI, build the snapshot from the source CSV:

```sh
go build -o snapshot-build ./cmd/snapshot-build

./snapshot-build \
  --rules=./rules-source/rules.csv \
  --model=v3-rc1 \
  --out=./artifacts/rules-v3-rc1.json
```

`snapshot-build` writes a JSON file that wraps bre-go's indexed `Snapshot` with the markup-side `Factors` map. The output:

```
snapshot-build: wrote 4823 rules (model=v3-rc1) to ./artifacts/rules-v3-rc1.json
```

Ship the snapshot file (not the CSV) to production along with the binary. Boot `markup-server` against it:

```sh
./markup-server \
  --snapshot=/etc/markup/rules-v3-rc1.json \
  --listen=:8080
```

`--snapshot` is mutually exclusive with `--rules` and `--route`. The snapshot's `ModelVersion` ("v3-rc1") overrides the `--model` flag. The active adapter is always `indexed` in snapshot mode — that is the only adapter that ships a snapshot format.

Quick smoke:

```sh
curl -sS -X POST -H 'Content-Type: application/json' \
  -d '{"customer_tier":"enterprise","country":"BR"}' \
  http://markup-svc.internal:8080/decide
# model_version in the response should be "v3-rc1"
# engine_adapter should be "*indexed.Engine"
```

## What's happening

The snapshot file embeds bre-go's compiled rule structure (the typed `parser.Condition` tree, the indexed buckets, the rule metadata) plus the per-rule markup factor map that bre-go's snapshot itself cannot serialize. `snapshot.LoadIntoIndexedDecider` reconstitutes a fully-built `indexed.Decider` directly without re-running `parser.ParseToCondition` on the condition expressions. See [ADR-0007](../architecture/decisions/0007-snapshot-persistence.md).

The cold-start cost tradeoff between the CSV path and the snapshot path is rule-set-size-dependent. At the 50-rule fixture the [scientific harness](../../scientific/v0.1.0/REPORT.md) measured, the snapshot path is roughly 3× slower than the CSV path because JSON-decode allocations dominate the parser savings. The harness's analysis names the crossover (where parser cost finally outweighs JSON decode at larger rule-set sizes) as a measurement gap — theorized but not yet measured. If your rule set is in the tens or low hundreds, measure your own boot times before committing to the snapshot promotion pipeline; at large rule-set sizes the snapshot path's win is intuitive but not yet quantified by the harness.

Format version is enforced: a snapshot with a `FormatVersion` other than the binary's known version fails at boot with `ErrFormatVersionMismatch`. An older binary cannot accidentally load a snapshot built by a newer binary.

## What to check after

- Boot log line names the snapshot path and the embedded model version: `markup-server: listening on :8080 (4823 rules, model v3-rc1, adapter indexed, source /etc/markup/rules-v3-rc1.json)`.
- `/decide` returns `"model_version":"v3-rc1"` regardless of the `--model` flag (the snapshot's ModelVersion wins).
- A corrupted snapshot file fails fast: `markup-server: read snapshot "/path/to/broken.json": ...` and the process exits before the listener opens.
- Snapshot file size: at production rule-set sizes the JSON file is hundreds of KB to a few MB. Fits in container images and Kubernetes ConfigMaps without ceremony.

## CI pipeline pattern

```yaml
# .github/workflows/build-snapshot.yml (or your CI's equivalent)
- run: go build -o snapshot-build ./cmd/snapshot-build
- run: ./snapshot-build --rules=rules-source/rules.csv --model=${{ github.sha }} --out=artifacts/rules.json
- uses: actions/upload-artifact@v4
  with:
    name: rules-snapshot
    path: artifacts/rules.json
```

Downstream deploy pulls the artifact, drops it at `/etc/markup/rules.json`, and the rolling restart picks it up.

## Mistakes to avoid

- **Editing the snapshot JSON by hand.** The JSON is a serialization format, not a source format. Edit the CSV, rebuild the snapshot.
- **Mixing `--snapshot` with `--rules` or `--route`.** They are mutually exclusive; the binary fails boot if you set more than one.
- **Hot-reloading a snapshot-mode server with an updated CSV.** The `--snapshot` boot does not know about a CSV path. To pick up new rules, rebuild the snapshot (CI step above) and either restart or use the reload endpoint with the new snapshot file at the same path the server was booted with.

## Relevant ADRs and flags

- [ADR-0007](../architecture/decisions/0007-snapshot-persistence.md) — the snapshot format and the rebuild semantics
- `--snapshot`, mutually exclusive with `--rules` and `--route`
- `cmd/snapshot-build --rules --model --out`
