# Deploy markup-svc on Kubernetes

## Problem

You want to ship `markup-server` on a Kubernetes cluster from the published container image, with declarative manifests under version control, sane probes wired up, and a path to scale + reload without ad-hoc kubectl commands.

## Recipe

The kustomize base lives at [`deploy/k8s/`](../../deploy/k8s/). Either apply it directly or overlay it from your own kustomize directory.

### First-time apply

```sh
git clone https://github.com/helmedeiros/markup-svc.git
cd markup-svc

# Create the namespace and resources.
kubectl create namespace markup-svc
kubectl apply -k deploy/k8s/
```

Five resources land in the `markup-svc` namespace: ConfigMap (`markup-rules`), Deployment (`markup-server`, 2 replicas), Service (`markup-server`, ClusterIP on 8080), HPA (`markup-server`, 2-10 replicas at 70% CPU). The Deployment pulls `ghcr.io/helmedeiros/markup-svc:main` by default.

### Smoke test

Once both pods are Ready (kubectl rolls them out in ~10s):

```sh
kubectl -n markup-svc port-forward svc/markup-server 8080:8080 &
curl -sS -X POST \
  -H 'Content-Type: application/json' \
  -H 'X-Correlation-ID: smoke-1' \
  -d '{"customer_tier":"enterprise","country":"BR"}' \
  http://localhost:8080/decide
```

Expected response:

```json
{
  "markup_factor": 1.15,
  "rule": "enterprise",
  "model_version": "v1",
  "correlation_id": "smoke-1",
  "engine_adapter": "*indexed.Engine"
}
```

### Updating the rule set

Edit the ConfigMap (in-place for a quick fix, or via your GitOps pipeline for a tracked change):

```sh
kubectl -n markup-svc edit configmap markup-rules
```

ConfigMap mount changes propagate to the pods within ~60s. Hot-reload picks up the new content immediately:

```sh
curl -sS -X POST http://localhost:8080/admin/reload
# {"rule_count":3,"model_version":"v1"}
```

See [hot-reload.md](hot-reload.md) for the full reload contract.

### Scaling

The HPA scales on CPU automatically (min 2, max 10 by default). For manual scaling or to adjust the ceiling, edit the HPA:

```sh
kubectl -n markup-svc scale --replicas=5 deployment/markup-server   # manual override
kubectl -n markup-svc edit hpa markup-server                        # adjust min/max/target
```

## What's happening

The Deployment runs `markup-server` from a distroless static image (~10 MiB) as user 65532 with a read-only root filesystem and all Linux capabilities dropped. Two replicas with a `maxUnavailable: 0` rolling update mean traffic never drops to zero during a rollout — the new pod becomes Ready (via the readinessProbe gating on `/readyz`) before the old pod is terminated.

The ConfigMap mounts `rules.csv` at `/etc/markup/rules.csv` and the server is started with `--rules=/etc/markup/rules.csv`. Operators with larger rule sets switch to an InitContainer pattern (see the appendix below); the 1 MiB ConfigMap hard cap is the trigger.

`/healthz` returns 200 while the HTTP goroutine is scheduled — kubelet restarts the pod on three consecutive probe failures so a deadlocked goroutine is recovered automatically. `/readyz` returns 200 only after the initial Decider construction succeeds; a misconfigured `--rules` path or a malformed CSV exits the process before the listener opens, so a pod whose `/readyz` is wedged on 503 indicates a deeper problem (volume mount missing, image regression, etc.) that the operator investigates via `kubectl logs`.

The HPA scales on CPU because the scientific harness's measured workload (~440 ns / Decide on indexed = ~2.26 M Decide/s/core) is CPU-bound at high QPS. Memory pressure is bounded by the rule set size + the request/response JSON buffers; the 256 MiB limit is generous for the markup workload.

## What to check after

- All pods Ready: `kubectl -n markup-svc get pods` shows `READY 1/1` for every pod. A pod stuck on `0/1` is failing readiness — `kubectl describe pod` shows which probe is failing.
- HPA is healthy: `kubectl -n markup-svc get hpa` shows `TARGETS: <N>%/70%` and a sane `REPLICAS` count. `unknown/70%` means metrics-server is not installed in your cluster.
- Service has endpoints: `kubectl -n markup-svc get endpoints markup-server` shows the IPs of the ready pods. Empty means no pods are Ready and no traffic will route.
- `/decide` works through port-forward (recipe above) AND through whatever Ingress / Gateway you wire in front of the Service.
- A failed rule update (e.g., malformed CSV in the ConfigMap) keeps the pods serving the OLD rule set because `/admin/reload` returns 500 and `holder.Swap` does NOT run on loader failure. The pods stay Ready throughout; the operator sees the failure in logs and reverts the ConfigMap.

## Appendix: large rule sets (> 1 MiB)

The Kubernetes ConfigMap hard cap is 1 MiB. For rule sets larger than that, replace the ConfigMap mount with an InitContainer that pulls the rule file from object storage:

```yaml
# Patch over the base deployment.yaml in your overlay
spec:
  template:
    spec:
      initContainers:
        - name: fetch-rules
          image: amazon/aws-cli:2
          command: ["aws", "s3", "cp", "s3://your-bucket/rules.csv", "/rules/rules.csv"]
          volumeMounts:
            - name: rules
              mountPath: /rules
      volumes:
        - name: rules
          emptyDir: {}                   # replaces the ConfigMap volume
```

The same path inside the container (`/etc/markup/rules.csv` if you mount the `emptyDir` there) keeps `--rules=...` unchanged.

For the snapshot path (see [snapshot-promotion.md](snapshot-promotion.md)), the InitContainer fetches the snapshot JSON instead and the Deployment runs `--snapshot=/etc/markup/snapshot.json` — the rest of the manifest is identical.

## Mistakes to avoid

- **Forgetting metrics-server.** The HPA needs the Metrics API to read CPU utilisation. On EKS / GKE / AKS this is installed by default; on a bare cluster you install `metrics-server` separately or the HPA stays at `unknown/70%` and never scales.
- **Setting `Recreate` rolling strategy on small clusters.** The default `RollingUpdate` is what makes traffic loss-free. `Recreate` (every pod terminated before the new pod starts) is a regression for any non-trivial service.
- **Mounting the ConfigMap as a `subPath`.** `subPath` mounts do NOT update when the ConfigMap changes — the file becomes a static copy. The base manifests use a plain `volumeMounts.mountPath` so ConfigMap updates propagate.
- **Exposing the Service as `LoadBalancer`.** That puts `/admin/reload` on the public internet without auth. Use `ClusterIP` (the default in the base) and put auth + rate-limiting between the public traffic and the Service via Ingress / Gateway.

## Relevant ADRs and flags

- [ADR-0013](../architecture/decisions/0013-production-deploy.md) — Dockerfile + k8s manifests rationale
- [ADR-0003](../architecture/decisions/0003-http-decide-route.md) — the HTTP contract the probes mount alongside
- [ADR-0008](../architecture/decisions/0008-hot-reload.md) — `/admin/reload` semantics on the ConfigMap update flow
- All cmd/markup-server flags (`--rules`, `--snapshot`, `--listen`, `--model`, `--adapter`, `--route`, `--policy`, `--otel-enabled`) work unchanged inside the container; the manifests pass them as `args:` in the Deployment spec.
