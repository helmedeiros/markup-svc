// Package httpapi serves the markup service over HTTP. Wire types
// (decideRequest, decideResponse) are unexported so the JSON
// serialisation contract stays out of the domain port (markup.Request
// and markup.Decision). See ADR-0003 for the design rationale.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

type decideRequest struct {
	ProductID    string  `json:"product_id"`
	Category     string  `json:"category"`
	CustomerTier string  `json:"customer_tier"`
	Channel      string  `json:"channel"`
	Country      string  `json:"country"`
	Inventory    string  `json:"inventory"`
	TimeWindow   string  `json:"time_window"`
	Amount       float64 `json:"amount"`
}

func (dr decideRequest) toMarkupRequest() markup.Request {
	return markup.Request{
		ProductID:    dr.ProductID,
		Category:     dr.Category,
		CustomerTier: dr.CustomerTier,
		Channel:      dr.Channel,
		Country:      dr.Country,
		Inventory:    dr.Inventory,
		TimeWindow:   dr.TimeWindow,
		Amount:       dr.Amount,
	}
}

type decideResponse struct {
	MarkupFactor  float64 `json:"markup_factor"`
	Rule          string  `json:"rule"`
	ModelVersion  string  `json:"model_version"`
	Experiment    string  `json:"experiment,omitempty"`
	CorrelationID string  `json:"correlation_id"`
	EngineAdapter string  `json:"engine_adapter"`
}

func fromDecision(d markup.Decision) decideResponse {
	return decideResponse{
		MarkupFactor:  d.MarkupFactor,
		Rule:          d.Rule,
		ModelVersion:  d.ModelVersion,
		Experiment:    d.Experiment,
		CorrelationID: d.CorrelationID,
		EngineAdapter: d.EngineAdapter,
	}
}

type errorBody struct {
	Error string `json:"error"`
}

// Decide returns an http.Handler that accepts POST /decide with a JSON
// body, calls d.Decide, and writes the Decision (200) or maps the
// error per ADR-0003. The handler is closure-bound to d so different
// model versions or adapters can be mounted on different muxes.
func Decide(d markup.Decider) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var dr decideRequest
		if err := json.NewDecoder(r.Body).Decode(&dr); err != nil {
			writeError(w, http.StatusBadRequest, "malformed JSON body")
			return
		}
		req := dr.toMarkupRequest()
		decision, err := d.Decide(r.Context(), req)
		ctx := r.Context()
		if err != nil {
			if errors.Is(err, markup.ErrNoMatch) {
				*r = *r.WithContext(withDecisionContext(ctx, decisionLogEntry{request: req, noMatch: true}))
				writeError(w, http.StatusNotFound, "no rule matched")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal")
			return
		}
		*r = *r.WithContext(withDecisionContext(ctx, decisionLogEntry{request: req, decision: decision}))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fromDecision(decision))
	})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: msg})
}
