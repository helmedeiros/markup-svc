package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/helmedeiros/markup-svc/internal/decider/guardrails"
)

// GuardrailsConfig is the typed JSON shape for both directions of
// the admin endpoint. Operators POST this to set the active rule
// configuration; GET returns the same shape with the values currently
// being enforced.
//
// Each axis is nullable. Omitting `factor_range` means "no
// FactorRange rule"; omitting `allowed_countries` means "no
// AllowedCountries rule"; omitting `required_fields` means "no
// RequiredFields rule". An explicitly empty `allowed_countries: []`
// or `required_fields: []` is an error -- mirrors the boot-flag
// behavior where `--allowed-countries=` with no values fails boot.
//
// The endpoint uses DisallowUnknownFields on the POST decoder: a body
// carrying a field the binary does not recognize fails with 400. This
// is intentional -- operators running a newer config body against an
// older binary surface the mismatch loudly rather than silently
// losing intent. Schema additions are a forward-only break by design.
//
// Custom Rule implementations (operators who built a wrapper main
// against the Rule port) are NOT enumerated by GET. The admin
// endpoint manages only the three shipped rule types from ADR-0014.
type GuardrailsConfig struct {
	FactorRange      *FactorRangeConfig `json:"factor_range,omitempty"`
	AllowedCountries []string           `json:"allowed_countries,omitempty"`
	RequiredFields   []string           `json:"required_fields,omitempty"`
}

// FactorRangeConfig is the typed FactorSpec on the JSON surface.
type FactorRangeConfig struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// GuardrailsAdmin returns the handler for /admin/guardrails. POST
// reads the body, validates via guardrails.BuildRules, on success
// Replaces the Holder's rules and returns 200 with the new
// configuration. GET returns the current configuration -- only the
// three shipped Rule types (FactorRange / AllowedCountries /
// RequiredFields) appear in the response; operator-supplied custom
// Rule implementations are not enumerated. Non-POST and non-GET
// return 405 with Allow: GET, POST. Validation failures (malformed
// JSON, inverted factor range, empty list, unknown body field) return
// 400 with an opaque body; the previous configuration keeps serving
// in every failure case. errLog receives one line per validation
// failure so operators can debug from logs without the detail leaking
// through HTTP.
//
// See ADR-0015 for the design rationale.
func GuardrailsAdmin(holder *guardrails.Holder, errLog io.Writer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeGuardrailsConfig(w, snapshotToConfig(holder.Snapshot()))
		case http.MethodPost:
			postGuardrailsAdmin(w, r, holder, errLog)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})
}

func postGuardrailsAdmin(w http.ResponseWriter, r *http.Request, holder *guardrails.Holder, errLog io.Writer) {
	var cfg GuardrailsConfig
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		fmt.Fprintf(errLog, "guardrails admin: malformed body: %v\n", err)
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}

	spec := configToSpec(cfg)
	rules, err := guardrails.BuildRules(spec)
	if err != nil {
		fmt.Fprintf(errLog, "guardrails admin: invalid configuration: %v\n", err)
		switch {
		case errors.Is(err, guardrails.ErrFactorRangeInverted):
			writeError(w, http.StatusBadRequest, "min_factor must not exceed max_factor")
		case errors.Is(err, guardrails.ErrAllowedCountriesEmpty):
			writeError(w, http.StatusBadRequest, "allowed_countries set with no values")
		case errors.Is(err, guardrails.ErrRequiredFieldsEmpty):
			writeError(w, http.StatusBadRequest, "required_fields set with no values")
		default:
			writeError(w, http.StatusBadRequest, "invalid configuration")
		}
		return
	}

	holder.Replace(rules)
	writeGuardrailsConfig(w, snapshotToConfig(holder.Snapshot()))
}

// configToSpec translates the JSON shape to the package's RuleSpec.
// The presence semantic is preserved: nil pointer / nil slice = no
// rule of that type.
func configToSpec(cfg GuardrailsConfig) guardrails.RuleSpec {
	var spec guardrails.RuleSpec
	if cfg.FactorRange != nil {
		spec.Factor = &guardrails.FactorSpec{Min: cfg.FactorRange.Min, Max: cfg.FactorRange.Max}
	}
	if cfg.AllowedCountries != nil {
		spec.AllowedCountries = cfg.AllowedCountries
	}
	if cfg.RequiredFields != nil {
		spec.RequiredFields = cfg.RequiredFields
	}
	return spec
}

// snapshotToConfig translates a Rule slice back to the JSON shape so
// GET returns what the operator most recently set (or the boot
// configuration, if no POST has happened).
func snapshotToConfig(rules []guardrails.Rule) GuardrailsConfig {
	var cfg GuardrailsConfig
	for _, r := range rules {
		switch v := r.(type) {
		case guardrails.FactorRange:
			cfg.FactorRange = &FactorRangeConfig{Min: v.Min, Max: v.Max}
		case guardrails.AllowedCountries:
			cfg.AllowedCountries = append([]string(nil), v.Countries...)
		case guardrails.RequiredFields:
			cfg.RequiredFields = append([]string(nil), v.Fields...)
		}
		// Custom Rule implementations (operator wrapper mains) are
		// not enumerated -- the admin endpoint manages the three
		// shipped rule types only.
	}
	return cfg
}

func writeGuardrailsConfig(w http.ResponseWriter, cfg GuardrailsConfig) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cfg)
}
