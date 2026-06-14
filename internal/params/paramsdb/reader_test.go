package paramsdb_test

// reader_test.go locks the pgx -> params seam translation that moved out of the
// params core: pgx.ErrNoRows must surface as params.ErrNoActivePayload (the
// "no promotion = baseline" signal Resolve falls through on), a payload must
// pass through verbatim, and any other DB error must propagate unchanged.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/byjackchen/trade-tms-go/internal/params"
	"github.com/byjackchen/trade-tms-go/internal/params/paramsdb"
)

// fakeRow implements pgx.Row: it either errors, or scans the captured payload
// into the first dest (a *json.RawMessage).
type fakeRow struct {
	err     error
	payload json.RawMessage
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*json.RawMessage)) = r.payload
	return nil
}

// fakeQuerier returns a fixed row.
type fakeQuerier struct{ row pgx.Row }

func (q fakeQuerier) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row { return q.row }

func TestActivePayloadNoRowsMapsToSentinel(t *testing.T) {
	r := paramsdb.NewReader(fakeQuerier{row: fakeRow{err: pgx.ErrNoRows}})
	raw, err := r.ActivePayload(context.Background(), "sepa")
	if raw != nil {
		t.Errorf("payload = %s, want nil", raw)
	}
	if !errors.Is(err, params.ErrNoActivePayload) {
		t.Fatalf("err = %v, want params.ErrNoActivePayload", err)
	}
}

func TestActivePayloadPassesThroughPayload(t *testing.T) {
	want := json.RawMessage(`{"strategy":"sepa"}`)
	r := paramsdb.NewReader(fakeQuerier{row: fakeRow{payload: want}})
	got, err := r.ActivePayload(context.Background(), "sepa")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("payload = %s, want %s", got, want)
	}
}

func TestActivePayloadPropagatesOtherErrors(t *testing.T) {
	boom := errors.New("boom")
	r := paramsdb.NewReader(fakeQuerier{row: fakeRow{err: boom}})
	_, err := r.ActivePayload(context.Background(), "sepa")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if errors.Is(err, params.ErrNoActivePayload) {
		t.Errorf("a generic error must NOT be reported as the no-row sentinel")
	}
}
