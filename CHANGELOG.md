# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `internal/markup` package: typed `Request`, `Decision`, `Decider` interface, `ErrNoMatch` sentinel.
- `internal/decider/inmemory`: first concrete `Decider`, wraps bre-go's `engine/inmemory.Engine`. Typed `Rule` (Name / Condition / Factor); `New(rules, modelVersion)` propagates bre-go's add-rule errors; `Decide` returns `markup.ErrNoMatch` on miss and populates Decision provenance (Rule from last-matched, ModelVersion, CorrelationID via `engine.CorrelationIDFromContext`, EngineAdapter as concrete type name).
- `cmd/markup-server`: skeleton main package.
- ADR-0001 (Accepted): domain port — Decider interface for markup decisions.
- Dependency: `github.com/helmedeiros/bre-go v0.19.0` (first integration).
- Makefile (lint / vet / test / cover / check-adrs / ci-local).
- GitHub Actions CI workflow.
- Scripts: `check-adrs.sh` for ADR-index gate.
- Project gitignore (excludes `*.local.md`).
