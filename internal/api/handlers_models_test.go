package api

// handlers_models_test.go drives the /api/v1/models CRUD through httptest over
// the in-memory stubModelStore (seeded from model.SeedModels) and
// stubAuditWriter, so the contract runs without a DB. There is no Model-level
// optimize endpoint (model-level joint hyperopt is dropped from the product).

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelList(t *testing.T) {
	t.Run("auth required", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodGet, "/api/v1/models", nil, false)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
	t.Run("lists the seeded models", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodGet, "/api/v1/models", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		out := decodeBody(t, rec)
		models := out["models"].([]any)
		assert.Len(t, models, 5)
	})
}

func TestModelGet(t *testing.T) {
	t.Run("known id returns the model", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodGet, "/api/v1/models/sepa-only", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		out := decodeBody(t, rec)
		m := out["model"].(map[string]any)
		assert.Equal(t, "sepa-only", m["id"])
	})
	t.Run("unknown id is 404", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodGet, "/api/v1/models/no-such", nil, true)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
}

func TestModelCreate(t *testing.T) {
	body := `{"id":"sepa-pairs-7030","name":"SEPA + Pairs 70/30","cash_pct":0.0,` +
		`"risk":{"single_name_pct":0.5,"concentration_pct":0.4,"daily_loss_halt_pct":0.1},` +
		`"members":[{"strategy_id":"sepa","weight":0.7,"active":true},` +
		`{"strategy_id":"pairs","weight":0.3,"active":true}],"actor":"alice"}`

	t.Run("creates and audits", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/models", strings.NewReader(body), true)
		require.Equal(t, http.StatusCreated, rec.Code)
		assert.Contains(t, ts.models.models, "sepa-pairs-7030")
		require.Len(t, ts.auditLog.records, 1)
		assert.Equal(t, "model.create", ts.auditLog.records[0].Action)
		assert.Equal(t, "sepa-pairs-7030", ts.auditLog.records[0].EntityID)
		assert.Equal(t, "api:alice", ts.auditLog.records[0].Actor)
	})
	t.Run("duplicate id is 409", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/models",
			strings.NewReader(`{"id":"sepa-only","name":"dup","risk":{"single_name_pct":0.2,"concentration_pct":0.3,"daily_loss_halt_pct":0.05},"members":[{"strategy_id":"sepa","weight":1,"active":true}]}`), true)
		assert.Equal(t, http.StatusConflict, rec.Code)
	})
	t.Run("invalid model is 422", func(t *testing.T) {
		ts := newTestServer(t)
		// Σ active weights + cash = 1.5 > 1.
		rec := ts.do(t, http.MethodPost, "/api/v1/models",
			strings.NewReader(`{"id":"bad","name":"bad","cash_pct":0.5,"risk":{"single_name_pct":0.2,"concentration_pct":0.3,"daily_loss_halt_pct":0.05},"members":[{"strategy_id":"sepa","weight":1,"active":true}]}`), true)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	})
}

func TestModelUpdate(t *testing.T) {
	t.Run("updates an existing model", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPut, "/api/v1/models/sepa-only",
			strings.NewReader(`{"name":"SEPA Renamed","risk":{"single_name_pct":0.2,"concentration_pct":0.3,"daily_loss_halt_pct":0.05},"members":[{"strategy_id":"sepa","weight":1,"active":true}]}`), true)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "SEPA Renamed", ts.models.models["sepa-only"].Name)
		require.Len(t, ts.auditLog.records, 1)
		assert.Equal(t, "model.update", ts.auditLog.records[0].Action)
	})
	t.Run("body id mismatch is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPut, "/api/v1/models/sepa-only",
			strings.NewReader(`{"id":"pairs-only","name":"x","risk":{"single_name_pct":0.2,"concentration_pct":0.3,"daily_loss_halt_pct":0.05},"members":[{"strategy_id":"sepa","weight":1,"active":true}]}`), true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("unknown id is 404", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPut, "/api/v1/models/no-such",
			strings.NewReader(`{"name":"x","risk":{"single_name_pct":0.2,"concentration_pct":0.3,"daily_loss_halt_pct":0.05},"members":[{"strategy_id":"sepa","weight":1,"active":true}]}`), true)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
}

func TestModelDelete(t *testing.T) {
	t.Run("deletes and audits", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodDelete, "/api/v1/models/pairs-only?actor=carol", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.NotContains(t, ts.models.models, "pairs-only")
		require.Len(t, ts.auditLog.records, 1)
		assert.Equal(t, "model.delete", ts.auditLog.records[0].Action)
		assert.Equal(t, "api:carol", ts.auditLog.records[0].Actor)
	})
	t.Run("unknown id is 404", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodDelete, "/api/v1/models/no-such", nil, true)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
}
