# 13. Production Deployment Artifacts: Dockerfile + Kubernetes Manifests

## Status

Proposed — proposes a production-grade container image for `cmd/markup-server` and a set of vanilla Kubernetes manifests so any operator can `kubectl apply -k deploy/k8s/` and get a running service. Adds two minimal health endpoints (`/healthz`, `/readyz`) so the k8s probes have something honest to gate on. Image publication wires into the existing CI workflow on every push to `main` and on every tag.

## Context

Twelve Accepted ADRs ship the engine, the decorators, the snapshot path, hot reload, the router, the observability hooks, and the scientific harness. None of them say how to actually run the binary on a cluster.

The README's quickstart says `go build && ./markup-server`. That works for a laptop and for the e2e test harness. It does not say how to ship the binary in a container, how the rule set is delivered to the pod, what probes Kubernetes should hit, or what resource requests are sensible for a workload that the scientific harness measures at ~440 ns per `Decide` on the indexed adapter (roughly 2.2 M Decide/s/core).

This ADR commits to vanilla Kubernetes manifests and a distroless container image as the deployment baseline. The image and manifests are the **reference** posture — operators on serverless runtimes (Cloud Run, App Runner, Fargate), Nomad, or bare-metal can use them as inspiration rather than gospel.

Four design questions.

### 1. Container base image

Three candidates:

- **`scratch`** — empty image, the binary alone. Smallest attack surface, hardest to debug (no shell, no DNS resolver, no CA bundle).
- **`gcr.io/distroless/static-debian11`** — Google's distroless static. ~2 MiB base. Ships a non-root user (`65532:65532`), CA bundle, `/etc/passwd`. No shell, no package manager.
- **`alpine:3.18`** — ~7 MiB base. Has a shell (BusyBox), apk for installing tools. Easier to debug in-cluster (`kubectl exec`).

Pick **distroless static**:

1. The binary is pure Go (`CGO_ENABLED=0`); no shared libraries needed.
2. Production posture is "no shell, no debugger" — operators debug through logs, metrics, traces, and the Decision Event Stream telemetry, not through `kubectl exec`.
3. The pre-shipped non-root user means the manifest does not need to set `securityContext.runAsUser` explicitly to get non-root; it inherits.
4. The CA bundle means outbound TLS to OTLP collectors, image registries, or future external services works without extra steps.

The Dockerfile is multi-stage: a `golang:1.18` build stage compiles the binary (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64`), and the runtime stage is `gcr.io/distroless/static-debian11:nonroot` with the binary copied in.

### 2. How does the rule set reach the pod?

Three delivery patterns:

- **ConfigMap mount** — `kubectl create configmap markup-rules --from-file=rules.csv` then mount at `/etc/markup/rules.csv`. Declarative, GitOps-friendly, auditable. Limit: 1 MiB per ConfigMap (Kubernetes hard cap).
- **InitContainer + object storage** — InitContainer pulls the rule set (or snapshot) from S3 / GCS into a shared volume; the markup-server container reads from the shared path. Unlimited size, but more moving parts and cloud creds.
- **PersistentVolumeClaim** — shared file system across pods. Persistent, but stateful and complicates rolling restarts.

Pick **ConfigMap as the default**, with a sidebar note in the cookbook that operators with rule sets larger than ~1 MiB switch to the InitContainer pattern. Most production markup rule sets sit comfortably under 1 MiB; the few that do not are a documented escape hatch, not a forced choice.

For larger deployments using the snapshot path (ADR-0007), the same pattern applies — the snapshot JSON also fits the ConfigMap limit at moderate rule-set sizes.

### 3. Health endpoints: what do they actually check?

`/healthz` and `/readyz` are the de facto Kubernetes conventions. The honest semantics:

- **`/healthz` (liveness)**: the process is up and the HTTP server is responding. Returns `200 {"status":"ok"}`. Always succeeds as long as the goroutine that handles HTTP is scheduled. The point is to give the kubelet a fast probe that distinguishes "deadlocked goroutine" (probe never responds) from "everything fine" — kubelet kills and restarts the pod on probe-fail.
- **`/readyz` (readiness)**: the Decider built successfully at boot AND every route in router mode built successfully. Returns `200 {"status":"ready"}` on success, `503 {"status":"not_ready","reason":"<short>"}` if the Decider failed to build. The kubelet gates traffic on readiness; a pod whose Decider failed to build never receives `/decide` requests.

Both endpoints are mounted on the same `http.ServeMux` as `/decide` and `/admin/reload`. Both pass through the `WithCorrelationID` middleware for log correlation. Both reject non-`GET` with `405` + `Allow: GET` per the rest of the HTTP contract.

### 4. Resource requests and limits

The scientific harness measured the indexed adapter at 442 ns/op = ~2.26 M Decide/s/core. The full-stack decorator chain (metrics + otel + swap + indexed) measured at 731 ns/op = ~1.37 M Decide/s/core. At the workloads described in the Pricing Decision Platform architecture (10-50 M requests/day = 116-579 rps), a single 1-vCPU pod handles the entire workload with three orders of magnitude of headroom.

Sensible defaults:

| Resource | Request | Limit |
|---|---|---|
| CPU | 100m | 500m |
| Memory | 64 MiB | 256 MiB |

These let two pods (the minimum for any production deployment) comfortably handle 1000+ rps with the full decorator stack and leave headroom for the rule set, observability buffers, and Go's runtime. The HPA scales on CPU > 70% with `minReplicas: 2` and `maxReplicas: 10` as the default ceiling — operators with traffic spikes that exceed that adjust upward.

## Decision

`cmd/markup-server/Dockerfile` ships:

```dockerfile
FROM golang:1.18 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/markup-server ./cmd/markup-server

FROM gcr.io/distroless/static-debian11:nonroot
COPY --from=build /out/markup-server /usr/local/bin/markup-server
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/markup-server"]
```

`internal/httpapi` ships:

```go
func Healthz() http.Handler  // liveness probe
func Readyz(ready func() (string, bool)) http.Handler  // readiness probe
```

`Healthz` always returns `200 {"status":"ok"}` on `GET`, `405 Allow: GET` otherwise.

`Readyz` calls the `ready` closure on every probe. If `ready()` returns `true`, the handler responds `200 {"status":"ready"}`. If `ready()` returns `false`, the handler responds `503 {"status":"not_ready","reason":"<closure-supplied string>"}`. cmd/markup-server supplies a closure that returns `true` once the initial `wireHandler` / `wireRouterHandler` call has succeeded, and `false` while construction is still running.

`deploy/k8s/` ships a kustomize-friendly base:

```
deploy/k8s/
  kustomization.yaml        # base resources
  deployment.yaml           # markup-server Deployment
  service.yaml              # ClusterIP on 8080
  configmap-rules.yaml      # placeholder ConfigMap with a sample rules.csv
  hpa.yaml                  # HPA targeting 70% CPU
```

Probes in the Deployment:

```yaml
livenessProbe:
  httpGet: { path: /healthz, port: 8080 }
  initialDelaySeconds: 3
  periodSeconds: 5
readinessProbe:
  httpGet: { path: /readyz, port: 8080 }
  initialDelaySeconds: 1
  periodSeconds: 3
```

The image publishes to `ghcr.io/helmedeiros/markup-svc:<tag>` on every tag push and `:main` + `:sha-<short>` on every push to `main`. The CI workflow gains a new job using `docker/build-push-action@v5` and `docker/login-action@v3` against the `GITHUB_TOKEN`-backed registry.

A new cookbook recipe `docs/cookbook/k8s-deploy.md` walks the operator through `kubectl apply -k deploy/k8s/` with the sample ConfigMap, the smoke `curl` against the Service, and the expected probe behaviour.

## Consequences

### Closed by this ADR

- `cmd/markup-server` ships a distroless static container image published to a public registry on every tag.
- Kubernetes operators can deploy the service with `kubectl apply -k deploy/k8s/` and adjust the ConfigMap rule set.
- `/healthz` and `/readyz` give the kubelet honest signals to gate traffic on.
- The cookbook gains a Kubernetes recipe so the deploy.md general recipe has a concrete cluster walkthrough.

### NOT closed by this ADR

- Helm chart. Kustomize is the v0.1.2 default; a Helm chart is a separate ADR if an operator asks for it.
- Service mesh integration (Istio sidecar annotations, Linkerd injection). Operators on a mesh add the annotations to the Deployment themselves; the base manifests stay mesh-agnostic.
- Multi-arch image (linux/arm64 + linux/amd64). The image is amd64 only in this release; multi-arch needs the QEMU buildx setup and lands separately if operators on Graviton / Ampere ask.
- mTLS for `/admin/reload`. The endpoint is still unauthenticated; operators with internet-exposed clusters put the admin path behind a NetworkPolicy or a separate auth proxy. Auth is its own ADR (already deferred from ADR-0003 and ADR-0008).
- Image signing / SBOM generation. Operators with supply-chain requirements add `cosign` and `syft` to the CI workflow themselves; the baseline image is unsigned.
- Operator pattern (e.g., a `MarkupServer` CRD). Out of scope; operators on heavy GitOps use Argo CD or Flux against the kustomize base directly.

### Performance impact

The container image adds zero per-`Decide` overhead — it is the same Go binary, statically linked. The `/healthz` and `/readyz` handlers add one allocation and one map lookup per probe; at the default 3-second probe period they cost microseconds per minute, invisible against engine work.

The Deployment's CPU request (100m) is intentionally generous compared to the measured workload — a CPU-throttled pod whose Go GC stalls during a Decide is a worse failure mode than slightly over-provisioned. Operators who want tighter packing can drop the request to 50m once they have measured their own steady-state utilisation.

### Validation strategy

- `internal/httpapi`: unit tests for `Healthz` and `Readyz` using `httptest.NewRecorder`. Cover happy path (200), method-not-allowed (405 with Allow header), `Readyz` not-ready path (503 with the reason).
- `cmd/markup-server`: the existing e2e tests (which use `httptest.NewServer`) gain probe assertions — `GET /healthz` returns 200, `GET /readyz` returns 200 after `wireHandler` succeeds.
- Dockerfile build verification: a CI step builds the image locally (does not publish) and runs the resulting container with `--help` to confirm the binary exits cleanly. Catches `Dockerfile` regressions without depending on a registry.
- Image-publish workflow: gated on `if: github.event_name == 'push' && (github.ref == 'refs/heads/main' || startsWith(github.ref, 'refs/tags/'))` so PRs build the image (for verification) but only main and tags publish.
- k8s manifests: `kubectl --dry-run=client apply -f deploy/k8s/` in the CI workflow catches YAML drift without needing a real cluster.
- A new `docs/cookbook/k8s-deploy.md` walks an operator through the apply + smoke + scale + reload flow on a real cluster.
