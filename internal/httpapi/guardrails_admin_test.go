package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/guardrails"
	"github.com/helmedeiros/markup-svc/internal/httpapi"
)

func newAdminServer(t *testing.T, holder *guardrails.Holder) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	errLog := &bytes.Buffer{}
	srv := httptest.NewServer(httpapi.GuardrailsAdmin(holder, errLog))
	t.Cleanup(srv.Close)
	return srv, errLog
}

func TestGuardrailsAdminGETReturnsCurrentConfig(t *testing.T) {
	holder := guardrails.NewHolder(
		guardrails.FactorRange{Min: 0.5, Max: 3.0},
		guardrails.AllowedCountries{Countries: []string{"BR", "DE"}},
	)
	srv, _ := newAdminServer(t, holder)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got httpapi.GuardrailsConfig
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.FactorRange == nil || got.FactorRange.Min != 0.5 || got.FactorRange.Max != 3.0 {
		t.Errorf("FactorRange = %+v, want {Min:0.5 Max:3.0}", got.FactorRange)
	}
	if len(got.AllowedCountries) != 2 || got.AllowedCountries[0] != "BR" || got.AllowedCountries[1] != "DE" {
		t.Errorf("AllowedCountries = %v, want [BR DE]", got.AllowedCountries)
	}
	if got.RequiredFields != nil {
		t.Errorf("RequiredFields = %v, want nil (no rule of that type)", got.RequiredFields)
	}
}

func TestGuardrailsAdminGETOnEmptyHolderReturnsEmptyConfig(t *testing.T) {
	srv, _ := newAdminServer(t, guardrails.NewHolder())

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	// Empty config -- all fields omitted via omitempty -- should be "{}"
	if strings.TrimSpace(string(body)) != "{}" {
		t.Errorf("body = %q, want \"{}\"", string(body))
	}
}

func TestGuardrailsAdminPOSTReplacesRules(t *testing.T) {
	holder := guardrails.NewHolder(guardrails.FactorRange{Min: 0.0, Max: 3.0})
	srv, _ := newAdminServer(t, holder)

	body := `{"factor_range":{"min":0.0,"max":1.0},"allowed_countries":["BR"]}`
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	snap := holder.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("len(snap) = %d, want 2 (FactorRange + AllowedCountries)", len(snap))
	}
	fr, ok := snap[0].(guardrails.FactorRange)
	if !ok || fr.Max != 1.0 {
		t.Errorf("snap[0] = %+v, want FactorRange{Max:1.0}", snap[0])
	}
}

func TestGuardrailsAdminPOSTInvalidJSONReturns400(t *testing.T) {
	holder := guardrails.NewHolder(guardrails.FactorRange{Min: 0.0, Max: 3.0})
	srv, errLog := newAdminServer(t, holder)

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{not json`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	// Holder must NOT have been mutated.
	if len(holder.Snapshot()) != 1 {
		t.Errorf("Snapshot mutated after malformed POST; len = %d, want 1", len(holder.Snapshot()))
	}
	if !strings.Contains(errLog.String(), "malformed body") {
		t.Errorf("errLog = %q, want it to mention 'malformed body'", errLog.String())
	}
}

func TestGuardrailsAdminPOSTInvertedFactorRangeReturns400(t *testing.T) {
	holder := guardrails.NewHolder(guardrails.FactorRange{Min: 0.0, Max: 3.0})
	srv, _ := newAdminServer(t, holder)

	resp, err := http.Post(srv.URL, "application/json",
		strings.NewReader(`{"factor_range":{"min":3.0,"max":1.0}}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "min_factor must not exceed max_factor") {
		t.Errorf("body = %q, want it to name the failing axis", string(body))
	}
	// Holder unchanged.
	snap := holder.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("len(snap) = %d, want 1", len(snap))
	}
	if fr, _ := snap[0].(guardrails.FactorRange); fr.Max != 3.0 {
		t.Errorf("Holder rule mutated; want Max=3.0, got %+v", fr)
	}
}

func TestGuardrailsAdminPOSTEmptyAllowedCountriesReturns400(t *testing.T) {
	holder := guardrails.NewHolder()
	srv, _ := newAdminServer(t, holder)

	resp, err := http.Post(srv.URL, "application/json",
		strings.NewReader(`{"allowed_countries":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestGuardrailsAdminPOSTUnknownFieldReturns400(t *testing.T) {
	holder := guardrails.NewHolder()
	srv, _ := newAdminServer(t, holder)

	resp, err := http.Post(srv.URL, "application/json",
		strings.NewReader(`{"factor_range":{"min":0.5,"max":3.0},"extra":"oops"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (DisallowUnknownFields)", resp.StatusCode)
	}
}

func TestGuardrailsAdminWrongMethodReturns405(t *testing.T) {
	srv, _ := newAdminServer(t, guardrails.NewHolder())

	req, _ := http.NewRequest(http.MethodDelete, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, POST" {
		t.Errorf("Allow header = %q, want %q", got, "GET, POST")
	}
}

func TestGuardrailsAdminConcurrentGETPOST(t *testing.T) {
	holder := guardrails.NewHolder(guardrails.FactorRange{Min: 0.0, Max: 3.0})
	srv, _ := newAdminServer(t, holder)

	const (
		getters       = 8
		posters       = 4
		opsPerWorker  = 200
		postBodyA     = `{"factor_range":{"min":0.0,"max":1.0}}`
		postBodyB     = `{"factor_range":{"min":0.0,"max":3.0}}`
	)
	var wg sync.WaitGroup
	wg.Add(getters + posters)
	for g := 0; g < getters; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				resp, err := http.Get(srv.URL)
				if err != nil {
					t.Errorf("GET: %v", err)
					return
				}
				if resp.StatusCode != http.StatusOK {
					t.Errorf("GET status = %d, want 200", resp.StatusCode)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	for p := 0; p < posters; p++ {
		body := postBodyA
		if p%2 == 1 {
			body = postBodyB
		}
		go func(b string) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				resp, err := http.Post(srv.URL, "application/json", strings.NewReader(b))
				if err != nil {
					t.Errorf("POST: %v", err)
					return
				}
				if resp.StatusCode != http.StatusOK {
					t.Errorf("POST status = %d, want 200", resp.StatusCode)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}(body)
	}
	wg.Wait()
}

func TestGuardrailsAdminRoundTripGETPOSTGET(t *testing.T) {
	holder := guardrails.NewHolder()
	srv, _ := newAdminServer(t, holder)

	// GET on empty: "{}"
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("initial GET: %v", err)
	}
	body1, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.TrimSpace(string(body1)) != "{}" {
		t.Errorf("initial GET body = %q, want {}", string(body1))
	}

	// POST: set a factor range + required fields.
	postBody := `{"factor_range":{"min":0.5,"max":3.0},"required_fields":["country"]}`
	resp, err = http.Post(srv.URL, "application/json", strings.NewReader(postBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()

	// GET: returns exactly what was POSTed.
	resp, err = http.Get(srv.URL)
	if err != nil {
		t.Fatalf("second GET: %v", err)
	}
	defer resp.Body.Close()
	var got httpapi.GuardrailsConfig
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.FactorRange == nil || got.FactorRange.Min != 0.5 || got.FactorRange.Max != 3.0 {
		t.Errorf("FactorRange after round trip = %+v, want {Min:0.5 Max:3.0}", got.FactorRange)
	}
	if len(got.RequiredFields) != 1 || got.RequiredFields[0] != "country" {
		t.Errorf("RequiredFields after round trip = %v, want [country]", got.RequiredFields)
	}
}
