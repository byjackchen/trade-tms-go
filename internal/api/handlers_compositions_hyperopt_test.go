package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/composition"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt/study"
	"github.com/byjackchen/trade-tms-go/internal/jobs/handlers"
)

// ---------------------------------------------------------------------------
// POST /api/v1/compositions/{id}/hyperopt
// ---------------------------------------------------------------------------

func TestCompositionHyperoptEnqueue(t *testing.T) {
	ts := newTestServer(t)
	// sector-only has no SEPA member, so no stock universe is required.
	body := `{"start":"2023-01-02","end":"2023-12-29","population":4,"generations":2}`
	rec := ts.do(t, http.MethodPost, "/api/v1/compositions/sector-only/hyperopt", strings.NewReader(body), true)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	require.Len(t, ts.jobs.enqueued, 1)
	job := ts.jobs.enqueued[0]
	require.Equal(t, handlers.KindHyperoptRun, job.Kind)
	payload := job.Payload.(map[string]any)
	require.Equal(t, string(study.KindComposition), payload["kind"])
	require.Equal(t, "sector-only", payload["composition_id"])
	require.Contains(t, payload, "composition") // the resolved blueprint
}

func TestCompositionHyperoptEnqueueWithRangeOverrides(t *testing.T) {
	ts := newTestServer(t)
	body := `{"start":"2023-01-02","end":"2023-12-29","single_name":[0.2,0.5],"cash":[0.0,0.2]}`
	rec := ts.do(t, http.MethodPost, "/api/v1/compositions/sector-only/hyperopt", strings.NewReader(body), true)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	require.Len(t, ts.jobs.enqueued, 1)
	payload := ts.jobs.enqueued[0].Payload.(map[string]any)
	ranges, ok := payload["ranges"].(map[string]any)
	require.True(t, ok, "ranges override must be packed into the payload")
	require.Contains(t, ranges, "single_name")
	require.Contains(t, ranges, "cash")
}

func TestCompositionHyperoptEnqueueSEPARequiresUniverse(t *testing.T) {
	ts := newTestServer(t)
	// sepa-only has an active SEPA member -> needs a stock universe.
	body := `{"start":"2023-01-02","end":"2023-12-29"}`
	rec := ts.do(t, http.MethodPost, "/api/v1/compositions/sepa-only/hyperopt", strings.NewReader(body), true)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, CodeValidation, errCode(t, rec))
}

func TestCompositionHyperoptEnqueueUnknownComposition(t *testing.T) {
	ts := newTestServer(t)
	body := `{"start":"2023-01-02","end":"2023-12-29"}`
	rec := ts.do(t, http.MethodPost, "/api/v1/compositions/does-not-exist/hyperopt", strings.NewReader(body), true)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestCompositionHyperoptEnqueueRequiresWindow(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodPost, "/api/v1/compositions/sector-only/hyperopt", strings.NewReader(`{}`), true)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// ---------------------------------------------------------------------------
// POST /api/v1/compositions/{id}/hyperopt/{study_ts}/promote
// ---------------------------------------------------------------------------

func TestCompositionHyperoptPromote(t *testing.T) {
	ts := newTestServer(t)
	ts.promoter.compOut = &study.PromotedComposition{
		CompositionID: "sector-only",
		CashPct:       0.2,
		Risk:          composition.Risk{SingleNamePct: 0.35, ConcentrationPct: 0.45, DailyLossHaltPct: 0.06},
		Weights:       map[string]float64{"sector_rotation": 1.0},
		Version:       2,
	}
	body := `{"trial_id":3,"actor":"alice"}`
	rec := ts.do(t, http.MethodPost,
		"/api/v1/compositions/sector-only/hyperopt/2026-06-13_12-00-00/promote", strings.NewReader(body), true)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The promoter saw the path id + study_ts + trial.
	require.Equal(t, "sector-only", ts.promoter.lastCompIn.CompositionID)
	require.Equal(t, "2026-06-13_12-00-00", ts.promoter.lastCompIn.StudyTS)
	require.Equal(t, 3, ts.promoter.lastCompIn.TrialNumber)

	m := decodeBody(t, rec)
	promoted := m["promoted"].(map[string]any)
	require.Equal(t, 0.2, promoted["cash_pct"])
	require.Equal(t, float64(2), promoted["version"])

	// The mutation was audited.
	require.NotEmpty(t, ts.auditLog.records)
}

func TestCompositionHyperoptPromoteRequiresTrialID(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodPost,
		"/api/v1/compositions/sector-only/hyperopt/2026-06-13_12-00-00/promote", strings.NewReader(`{}`), true)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCompositionHyperoptPromoteStudyNotFound(t *testing.T) {
	ts := newTestServer(t)
	ts.promoter.notFound = true
	rec := ts.do(t, http.MethodPost,
		"/api/v1/compositions/sector-only/hyperopt/2026-06-13_12-00-00/promote",
		strings.NewReader(`{"trial_id":1}`), true)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestCompositionHyperoptPromoteBadStudyTS(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodPost,
		"/api/v1/compositions/sector-only/hyperopt/not-a-ts/promote",
		strings.NewReader(`{"trial_id":1}`), true)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}
