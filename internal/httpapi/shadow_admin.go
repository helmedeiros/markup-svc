package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

// ChallengerHolder is the minimum contract LoadChallenger and
// ClearChallenger require. internal/decider/shadow.Holder satisfies
// it; tests substitute fakes without importing the adapter.
type ChallengerHolder interface {
	Load(markup.Decider)
	Clear()
	Get() (markup.Decider, bool)
}

// LoadChallenger handles POST /admin/load-challenger. On success it
// installs a new challenger Decider into holder and returns the same
// {rule_count, model_version} envelope as /admin/reload. A Diagnose
// failure returns 400 with the ADR-0026 envelope.
func LoadChallenger(holder ChallengerHolder, body ReloadBodyLoader) http.Handler {
	if holder == nil {
		panic("httpapi.LoadChallenger: holder is required")
	}
	if body == nil {
		panic("httpapi.LoadChallenger: body loader is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		mediaType, _, mtErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mtErr != nil || !body.Supports(mediaType) {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported media type")
			return
		}
		raw, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, defaultMaxBodyBytes))
		if readErr != nil {
			if readErr.Error() == "http: request body too large" {
				writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds limit")
				return
			}
			writeError(w, http.StatusInternalServerError, "read body")
			return
		}
		if len(raw) == 0 {
			writeError(w, http.StatusBadRequest, "empty body")
			return
		}
		decider, result, err := body.Load(mediaType, raw)
		var dre *DiagnoseRejectedError
		if errors.As(err, &dre) {
			writeDiagnoseRejection(w, dre.Diagnosis)
			return
		}
		if err != nil {
			writeError(w, statusForLoadErr(err), "load challenger failed")
			return
		}
		holder.Load(decider)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reloadResponse(result))
	})
}

// ClearChallenger handles DELETE /admin/challenger. Returns 204
// whether or not a challenger was loaded; idempotent.
func ClearChallenger(holder ChallengerHolder) http.Handler {
	if holder == nil {
		panic("httpapi.ClearChallenger: holder is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.Header().Set("Allow", http.MethodDelete)
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		holder.Clear()
		w.WriteHeader(http.StatusNoContent)
	})
}
