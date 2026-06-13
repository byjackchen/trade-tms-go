package api

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog"
)

// Error codes used in the error envelope (documented in docs/api.md).
const (
	CodeUnauthorized = "unauthorized"
	CodeValidation   = "validation"
	CodeNotFound     = "not_found"
	CodeInternal     = "internal"
)

// errorBody is the uniform error envelope: {"error": {"code", "message"}}.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSON encodes v with the canonical headers. Encode errors after the
// header is written can only mean a vanished client; they are dropped.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeError writes the uniform error envelope.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: msg}})
}

// internalError logs the real error and answers with a generic message so
// internals (SQL, hosts) never leak to clients.
func internalError(w http.ResponseWriter, log zerolog.Logger, context string, err error) {
	log.Error().Err(err).Str("context", context).Msg("request failed")
	writeError(w, http.StatusInternalServerError, CodeInternal, "internal error; see server logs")
}
