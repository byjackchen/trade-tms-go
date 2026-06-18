package api

// handlers_accounts.go is the CRUD write surface for the user-managed accounts
// registry (POST/PATCH/DELETE /api/v1/trade/accounts). Accounts are first-class,
// edited from the UI — no longer derived from .env. All three are bearer-guarded
// (the router group) and mutate tms.accounts via the TradeAccountWriter.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// handleAccountCreate serves POST /api/v1/trade/accounts.
func (s *Server) handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	if s.accountWriter == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "account writer not configured")
		return
	}
	req, ok := decodeAccountBody(w, r)
	if !ok {
		return
	}
	acct, err := s.accountWriter.CreateAccount(r.Context(), req)
	if mapAccountErr(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, acct)
}

// handleAccountUpdate serves PATCH /api/v1/trade/accounts/{id}.
func (s *Server) handleAccountUpdate(w http.ResponseWriter, r *http.Request) {
	if s.accountWriter == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "account writer not configured")
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	var patch AccountPatchRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid account patch: "+err.Error())
		return
	}
	acct, err := s.accountWriter.UpdateAccount(r.Context(), id, patch)
	if mapAccountErr(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, acct)
}

// handleAccountDelete serves DELETE /api/v1/trade/accounts/{id}. Hard-delete;
// 409 when the account is still referenced (FK RESTRICT).
func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	if s.accountWriter == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "account writer not configured")
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if mapAccountErr(w, s.accountWriter.DeleteAccount(r.Context(), id)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeAccountBody strict-decodes a create/update body, writing 400 on failure.
func decodeAccountBody(w http.ResponseWriter, r *http.Request) (AccountWriteRequest, bool) {
	var req AccountWriteRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid account body: "+err.Error())
		return AccountWriteRequest{}, false
	}
	return req, true
}

// mapAccountErr maps a CRUD error to its HTTP status. Returns true when it wrote a
// response (the caller then returns); false when err is nil (caller proceeds).
func mapAccountErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrInvalidAccount):
		writeError(w, http.StatusBadRequest, "invalid_account", err.Error())
	case errors.Is(err, ErrAccountNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ErrAccountInUse):
		writeError(w, http.StatusConflict, "account_in_use", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", "account write failed: "+err.Error())
	}
	return true
}
