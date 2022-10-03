# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `internal/markup` package: typed `Request`, `Decision`, `Decider` interface, `ErrNoMatch` sentinel.
- `cmd/markup-server`: skeleton main package.
- ADR-0001 (Proposed): domain port — Decider interface for markup decisions.
- Makefile (lint / vet / test / cover / check-adrs / ci-local).
- GitHub Actions CI workflow.
- Scripts: `check-adrs.sh` for ADR-index gate.
- Project gitignore (excludes `*.local.md`).
