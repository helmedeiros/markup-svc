package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/helmedeiros/markup-svc/internal/decider/swap"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

// ReloadResult is the per-reload metadata the loader returns alongside
// the freshly-built Decider. Surfaces as JSON on the /admin/reload
// response so callers see what is now serving.
type ReloadResult struct {
	RuleCount    int
	ModelVersion string
}

// Loader rebuilds the active Decider from disk. cmd/markup-server
// supplies the closure that captures the original --rules or
// --snapshot path so reloads re-run the boot-time load path against
// the current file contents. The Loader type lives here so the
// handler signature does not depend on cmd's wiring.
type Loader func() (markup.Decider, ReloadResult, error)

type reloadResponse struct {
	RuleCount    int    `json:"rule_count"`
	ModelVersion string `json:"model_version"`
}

// ReloadOption customises Reload at construction time.
type ReloadOption func(*reloadConfig)

type reloadConfig struct {
	gate DiagnoseFn
}

// WithReloadDiagnose gates the reload on Diagnose: if the new rules
// fail diagnosis, the handler returns 400 + JSON of the issues and
// the active Decider is NOT swapped. The currently-serving rules
// keep handling /decide. See ADR-0026.
func WithReloadDiagnose(fn DiagnoseFn) ReloadOption {
	return func(c *reloadConfig) { c.gate = fn }
}

// Reload returns an http.Handler that, on POST, invokes loader() to
// build a fresh Decider, atomically swaps it into holder via Swap,
// and responds 200 with the new RuleCount + ModelVersion. Loader
// errors map to 500 with an opaque body so the underlying error does
// not leak into the response; the loader closure is expected to
// surface details via stderr or the configured logger. Non-POST
// returns 405 with Allow: POST per RFC 7231. See ADR-0008 + ADR-0026.
func Reload(holder *swap.Decider, loader Loader, opts ...ReloadOption) http.Handler {
	cfg := reloadConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if cfg.gate != nil {
			d, err := cfg.gate()
			if err != nil {
				writeError(w, statusForLoadErr(err), "diagnose: "+err.Error())
				return
			}
			if !d.IsHealthy() {
				writeDiagnoseRejection(w, d)
				return
			}
		}
		decider, result, err := loader()
		if err != nil {
			writeError(w, statusForLoadErr(err), "reload failed")
			return
		}
		holder.Swap(decider)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reloadResponse(result))
	})
}

func writeDiagnoseRejection(w http.ResponseWriter, d markup.Diagnosis) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":    "reload rejected: rule set failed Diagnose",
		"healthy":  false,
		"errors":   issuesToWire(d.Errors()),
		"warnings": issuesToWire(d.Warnings()),
	})
}

func statusForLoadErr(err error) int {
	var ire *markup.InvalidRuleSetError
	if errors.As(err, &ire) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func issuesToWire(in []markup.Issue) []responseIssue {
	out := make([]responseIssue, len(in))
	for i, x := range in {
		out[i] = responseIssue{Kind: x.Kind, Rule: x.Rule, Detail: x.Detail}
	}
	return out
}
