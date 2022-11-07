# Architecture Decision Records

Each file in this folder captures one architecture decision made on the markup-svc codebase, following the standard ADR shape (Status / Context / Decision / Consequences).

New decisions get the next number and a short kebab-case slug:

```
NNNN-short-decision-name.md
```

`scripts/check-adrs.sh` (wired into `make ci-local`) verifies that:

1. Every ADR file is indexed in this README.
2. Every README link points at a file that exists.
3. Every ADR file has a `## Status` line with one of: `Proposed`, `Accepted`, `Superseded by ADR-NNNN`, `Deprecated`.
4. Every ADR file has the four standard sections: `## Status`, `## Context`, `## Decision`, `## Consequences`.

## Index

| # | Title | Status |
|---|---|---|
| [0001](0001-domain-port.md) | Domain port: Decider interface for markup decisions | ✅ Accepted |
| [0002](0002-rule-format-csv.md) | Rule format: CSV with parser expressions | ✅ Accepted |
| [0003](0003-http-decide-route.md) | HTTP transport: POST /decide | ✅ Accepted |
| [0004](0004-firstmatch-adapter.md) | First-match Decider adapter | ✅ Accepted |
| [0005](0005-priority-adapter.md) | Priority Decider adapter | ✅ Accepted |
| [0006](0006-indexed-adapter.md) | Indexed Decider adapter | ✅ Accepted |
| [0007](0007-snapshot-persistence.md) | Snapshot persistence for the indexed adapter | 🟡 Proposed |
