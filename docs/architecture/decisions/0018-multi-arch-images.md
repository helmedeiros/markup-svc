# 18. Multi-arch (linux/amd64 + linux/arm64) image publish

## Status

Accepted — `cmd/markup-server/Dockerfile` builds with `--platform=$BUILDPLATFORM` on the build stage and cross-compiles via `GOARCH=${TARGETARCH:-amd64}`; the CI image-publish job passes `platforms: linux/amd64,linux/arm64` to `docker/build-push-action` so every published tag is a manifest list. Operators on Apple Silicon (`docker pull ghcr.io/helmedeiros/markup-svc:vN`) automatically receive the arm64 variant and skip the Rosetta-2 emulation penalty; Graviton-class AWS instances (`linux/arm64`) get the same image without a separate build pipeline.

## Context

ADR-0013 shipped the initial production deployment artifacts: a two-stage `Dockerfile` (golang:1.18 build → distroless static runtime), `:nonroot` user, Kubernetes manifests with the matching securityContext. The build stage hard-coded `GOOS=linux GOARCH=amd64`. That was fine for the deployment target at the time (production = linux/amd64 EC2 + GKE).

Two things changed.

1. **Dev environment is Apple Silicon.** The platform's `decision-gateway/docker-compose.yaml` runs the three published images locally for cookbook walkthroughs. On arm64 Docker Desktop, pulling an amd64-only image triggers a compatibility warning AND runs the binary under Rosetta-2 user-space translation. The cross-service trace we instrumented in ADR-0017 measured ~1.7ms of network round-trip + connection-pool overhead between traffic-gen → gateway → markup-svc in a 2.0ms total request; the gap between that 1.7ms (mostly emulation cost) and the underlying 23µs engine work hid every other measurement. The emulation tax is the single largest controllable cost in the dev-stack trace.
2. **arm64 is becoming a production target.** AWS Graviton (arm64) instances are now the default for cost-optimized fleets. Pinning to amd64-only images excludes a class of production deployment options for no portability reason — the Go binary itself has no architecture dependency.

Two design questions.

### 1. Cross-compile in one builder vs run two builders under QEMU

The standard buildx multi-arch path runs the Dockerfile once per target platform. By default that means running the `golang:1.18` build stage under QEMU emulation for the non-native target — slow (the build runs at ~5x slowdown under QEMU on a GitHub-hosted amd64 runner).

The faster path: pin the build stage to `--platform=$BUILDPLATFORM` so it always runs native, then use `GOARCH=$TARGETARCH` on the `go build` line to cross-compile. Go's compiler cross-compiles trivially when CGO is disabled (already the case for ADR-0013's `CGO_ENABLED=0`); QEMU is bypassed entirely.

**Pick cross-compile.** The CI build time stays roughly equal to the existing single-arch build (one native build stage, one cross-compile invocation) instead of doubling under QEMU. The Dockerfile change is two lines (`--platform=$BUILDPLATFORM` on the FROM, `GOARCH=${TARGETARCH:-amd64}` on the build).

### 2. Manifest list vs separate tags per arch

Two image-distribution shapes:

- **Manifest list** (`ghcr.io/helmedeiros/markup-svc:vN` points at a multi-platform manifest; `docker pull` selects automatically): one tag per release; operator workflow is unchanged. Industry standard.
- **Per-arch tags** (`:vN-amd64`, `:vN-arm64`): explicit; operators on arm64 must remember the suffix; cookbook recipes have to branch.

**Pick manifest list.** No operator-visible change at pull time. The CI step adds one line (`platforms: linux/amd64,linux/arm64`); the resulting ghcr.io tag is what `docker pull` resolves for both architectures.

## Decision

`cmd/markup-server/Dockerfile`:

- Build stage gains `--platform=$BUILDPLATFORM` on the `FROM golang:1.18` so it always runs on the runner's native arch.
- Build args `BUILDPLATFORM`, `TARGETOS`, `TARGETARCH` declared (provided automatically by buildx).
- `go build` line uses `GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64}` so a plain `docker build` (no buildx) still produces a working amd64-only image (the default).

`.github/workflows/ci.yml` image job:

- `docker/build-push-action@v5` step gains `platforms: linux/amd64,linux/arm64`.
- `docker/setup-buildx-action@v3` already present (from ADR-0013's CI work); no additional setup needed.
- `cache-from: type=gha` + `cache-to: type=gha,mode=max` already there; cache is keyed per-platform automatically by buildx.

The runtime image (`gcr.io/distroless/static-debian11:nonroot`) is already a multi-arch manifest (Google publishes amd64 + arm64 variants) so the FROM resolves to the matching arch via Docker's pull rules — no change needed there.

## Consequences

### Closed by this ADR

- `docker pull ghcr.io/helmedeiros/markup-svc:vN` on Apple Silicon returns the arm64 variant; the cookbook recipes work natively without Rosetta-2 emulation. The dev-stack trace's per-hop network cost drops to the actual Docker-bridge wire time (an order of magnitude lower than the emulated cost; expected ~50-100µs per hop instead of ~800µs).
- arm64 production targets (Graviton, Ampere-class bare metal) are unlocked with no separate CI pipeline.
- The published `:vN` tag stays the canonical reference in all compose files + cookbook recipes; operators do not change their pull commands.

### NOT closed by this ADR

- linux/amd64 + linux/arm64 only. linux/arm/v7 (32-bit ARM, Raspberry Pi-class) is not in the platforms list. Goes in when an operator's deployment target asks; the Dockerfile cross-compile path already handles GOARCH=arm so the change is one line.
- Windows / macOS containers. Out of scope (the distroless runtime is Linux-only).
- Per-platform image size verification. amd64 and arm64 binaries differ slightly in size due to instruction encoding; the CI does not assert a size budget. Lands if image-size regression becomes a problem.
- Reproducible builds across architectures. The `-trimpath` + `-ldflags="-s -w"` from ADR-0013 still give us bit-identical builds per-architecture, but the amd64 and arm64 binaries are independently bit-identical (they differ from each other by design). A future cross-arch reproducibility test would compare hash files per platform.

### Performance impact

- **CI build time**: the cross-compile path adds one extra `go build` invocation (~30 seconds for the second arch). The original amd64-only build took ~2 minutes; the multi-arch build takes ~2.5 minutes. The buildx GHA cache (`cache-to: type=gha,mode=max`) means subsequent runs hit on most layers, keeping the steady-state build time close to the original.
- **Pull time on Apple Silicon**: amd64-only image triggers a compatibility warning + Rosetta-2 runtime translation (which adds startup latency and per-syscall overhead). The arm64 variant runs natively — the dev-stack trace measurements become representative of native performance instead of emulated.
- **Runtime cost**: zero difference between native execution on amd64 and arm64 — Go's binary is the same workload. The improvement is purely the removal of the emulation layer when the host arch is arm64.

### Validation strategy

- `docker buildx build --platform linux/amd64,linux/arm64 -f cmd/markup-server/Dockerfile .` runs locally and produces a manifest list. Local verification by `docker buildx imagetools inspect <local-tag>`.
- CI builds + pushes both arches; `docker pull ghcr.io/helmedeiros/markup-svc:vN` on Apple Silicon resolves to the arm64 variant (check via `docker image inspect <id> --format '{{.Architecture}}'`).
- Integration smoke: bring up `decision-gateway/docker-compose.yaml` on Apple Silicon native; observe the absence of the "no matching manifest" + "Rosetta translation" warnings; observe the per-hop network cost in the Jaeger trace dropping by ~10x (sample from ADR-0017's instrumented trace becomes the baseline).
