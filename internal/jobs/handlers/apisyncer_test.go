package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/sharadar"
)

// fakeCatchup is a catchupEngine stub that records the call and returns a
// canned report/error.
type fakeCatchup struct {
	called int
	rep    *sharadar.CatchupReport
	err    error
}

func (f *fakeCatchup) EnsureFresh(context.Context) (*sharadar.CatchupReport, error) {
	f.called++
	return f.rep, f.err
}

func newAdapter(c catchupEngine) *SharadarAPISyncer {
	return &SharadarAPISyncer{syncer: c, log: zerolog.Nop()}
}

func TestNewSharadarAPISyncerNilSyncer(t *testing.T) {
	_, err := NewSharadarAPISyncer(nil, zerolog.Nop())
	require.Error(t, err)
}

func TestAPISyncerRunsCatchup(t *testing.T) {
	fc := &fakeCatchup{rep: &sharadar.CatchupReport{
		DaysAttempted: 3,
		DaysSucceeded: 3,
		RowsAdded: map[string]int64{
			sharadar.DatasetSEP: 6, sharadar.DatasetSFP: 3,
			sharadar.DatasetSF1: 50, sharadar.DatasetEvents: 20, sharadar.DatasetTickers: 100,
		},
	}}
	a := newAdapter(fc)

	var progressPhases []string
	report := func(_ context.Context, p any) error {
		if m, ok := p.(map[string]any); ok {
			if ph, ok := m["phase"].(string); ok {
				progressPhases = append(progressPhases, ph)
			}
		}
		return nil
	}

	result, err := a.Sync(context.Background(), APISyncRequest{}, report)
	require.NoError(t, err)
	require.Equal(t, 1, fc.called, "EnsureFresh must be invoked exactly once")

	m, ok := result.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "api", m["source"])
	assert.Equal(t, "catchup", m["flow"])
	assert.Equal(t, true, m["did_work"])
	assert.Equal(t, 3, m["days_attempted"])
	assert.Equal(t, 3, m["days_succeeded"])
	assert.Equal(t, int64(6), m["rows_added"].(map[string]int64)[sharadar.DatasetSEP])

	// Progress must announce the catchup start and a terminal done frame.
	assert.Contains(t, progressPhases, "catchup")
	assert.Contains(t, progressPhases, "done")
}

func TestAPISyncerSkippedNotBootstrapped(t *testing.T) {
	fc := &fakeCatchup{rep: &sharadar.CatchupReport{SkippedReason: "not-bootstrapped"}}
	a := newAdapter(fc)

	result, err := a.Sync(context.Background(), APISyncRequest{}, noProgress)
	require.NoError(t, err) // skip is not an error — operator must bootstrap
	m := result.(map[string]any)
	assert.Equal(t, "not-bootstrapped", m["skipped_reason"])
	assert.Equal(t, false, m["did_work"])
}

func TestAPISyncerPropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	fc := &fakeCatchup{rep: &sharadar.CatchupReport{}, err: sentinel}
	a := newAdapter(fc)

	_, err := a.Sync(context.Background(), APISyncRequest{}, noProgress)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.Contains(t, err.Error(), "api catchup")
}

// TestAPISyncerRejectsScopedRequests guards the routing contract: catchup is
// whole-universe + watermark-driven, so a scoped source=api job (the exact
// mis-route in the FIXER finding: table SEP, AAPL/KO, since 2026-06-06) must
// fail fast pointing at `tms sync bootstrap`, never silently ignore scope.
func TestAPISyncerRejectsScopedRequests(t *testing.T) {
	cases := []struct {
		name string
		req  APISyncRequest
		want string
	}{
		{"tables", APISyncRequest{Tables: []string{"SEP"}}, "tables=[SEP]"},
		{"tickers", APISyncRequest{Tickers: []string{"AAPL", "KO"}}, "2 ticker(s)"},
		{"since", APISyncRequest{Since: time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)}, "since=2026-06-06"},
		{
			"the finding repro",
			APISyncRequest{Tables: []string{"SEP"}, Tickers: []string{"AAPL", "KO"}, Since: time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)},
			"tms sync bootstrap",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeCatchup{}
			a := newAdapter(fc)
			_, err := a.Sync(context.Background(), tc.req, noProgress)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "tms sync bootstrap")
			assert.Equal(t, 0, fc.called, "scoped request must not run catchup")
		})
	}
}

// TestAPISyncerNilReportSafe ensures a nil ProgressFn does not panic.
func TestAPISyncerNilReportSafe(t *testing.T) {
	fc := &fakeCatchup{rep: &sharadar.CatchupReport{}}
	a := newAdapter(fc)
	_, err := a.Sync(context.Background(), APISyncRequest{}, nil)
	require.NoError(t, err)
}
