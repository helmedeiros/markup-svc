package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

// DiagnoseFn re-runs the rule loader + Diagnose so the admin endpoint
// always reads the current rules file. cmd constructs it once at
// boot, closing over the loader path + adapter + model so neither
// the handler nor markup package depends on internal/load. See
// ADR-0025.
type DiagnoseFn func() (markup.Diagnosis, error)

// Diagnose mounts on GET /admin/diagnose. Returns the current
// Diagnosis as JSON with the http status reflecting healthiness:
// 200 when no errors are present (warnings still surface in the
// body); 503 when at least one error is present so an operator's
// curl-driven CI gate fails on a broken rule set without parsing
// the body.
func Diagnose(fn DiagnoseFn) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		d, err := fn()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "diagnose: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if !d.IsHealthy() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(toResponse(d))
	})
}

type diagnoseResponse struct {
	Healthy  bool              `json:"healthy"`
	Errors   []responseIssue   `json:"errors,omitempty"`
	Warnings []responseIssue   `json:"warnings,omitempty"`
}

type responseIssue struct {
	Kind   markup.IssueKind `json:"kind"`
	Rule   string           `json:"rule,omitempty"`
	Detail string           `json:"detail"`
}

func toResponse(d markup.Diagnosis) diagnoseResponse {
	conv := func(in []markup.Issue) []responseIssue {
		out := make([]responseIssue, len(in))
		for i, x := range in {
			out[i] = responseIssue{Kind: x.Kind, Rule: x.Rule, Detail: x.Detail}
		}
		return out
	}
	return diagnoseResponse{
		Healthy:  d.IsHealthy(),
		Errors:   conv(d.Errors()),
		Warnings: conv(d.Warnings()),
	}
}
