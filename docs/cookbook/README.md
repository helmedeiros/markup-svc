# markup-svc cookbook

Operator-level recipes for common deployments. Each recipe is one page, names the relevant ADRs and `cmd/markup-server` flags, and ends with a "what to check after" section so a reader who follows the commands has an obvious sanity check.

## Recipes

| Recipe | When to use |
|---|---|
| [deploy.md](deploy.md) | First-time production deployment of markup-svc |
| [ab-rollout.md](ab-rollout.md) | Roll a new model version out to a slice of traffic for comparison |
| [hot-reload.md](hot-reload.md) | Push a rule fix without restarting the process |
| [snapshot-promotion.md](snapshot-promotion.md) | Use the offline snapshot build to skip startup parsing |
| [multi-model.md](multi-model.md) | Serve more than one model version side-by-side from one binary |
| [observability.md](observability.md) | Wire OpenTelemetry spans and the metrics sink into your stack |

## How these recipes are written

Each recipe answers one operational question. The format is:

1. **Problem** — one sentence stating what the operator is trying to do.
2. **Recipe** — the commands and config, copy-paste-ready.
3. **What's happening** — one paragraph explaining the mechanism so the recipe is not a black box.
4. **What to check after** — concrete signals (log lines, response shapes, dashboard values) that confirm the recipe worked.
5. **Relevant ADRs and flags** — pointers into the design docs and the binary's flag list.

If a recipe and an ADR disagree, the ADR is the source of truth — file a follow-up to fix the recipe. ADRs ship with the code; recipes ship with operator practice.
