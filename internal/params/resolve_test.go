package params_test

// resolve_test.go covers the resolution precedence (DB -> file -> baseline),
// per-strategy baseline fallback, and document parsing of display/allocation.
// The DB tier is exercised with a fake PayloadReader so no live PostgreSQL is
// required (the pgx-backed reader's SQL + no-row->sentinel mapping lives in the
// internal/params/paramsdb adapter and is unit-tested there; the real-DB query
// path is the runs-package live-DB harness's concern).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/byjackchen/trade-tms-go/internal/params"
)

// fakeReader returns a fixed payload for one strategy, nil otherwise.
type fakeReader struct {
	strategy string
	payload  json.RawMessage
	err      error
}

func (f fakeReader) ActivePayload(_ context.Context, strategy string) (json.RawMessage, error) {
	if f.err != nil {
		return nil, f.err
	}
	if strategy == f.strategy {
		return f.payload, nil
	}
	return nil, nil // no active row -> fall through
}

// sepaDoc builds a valid sepa document with a tuned risk_pct, for DB/file tiers.
func sepaDoc(riskPct float64) []byte {
	doc := map[string]any{
		"strategy":       "sepa",
		"schema_version": 1,
		"allocation":     map[string]any{"capital_pct": 0.40, "active": true},
		"parameters": map[string]any{
			"risk_pct":                 map[string]any{"default": riskPct, "type": "float", "search": map[string]any{"low": 1.0, "high": 4.0}},
			"market_cap_min_usd":       map[string]any{"default": 5e8, "type": "float"},
			"hard_stop_pct":            map[string]any{"default": 7.5, "type": "float"},
			"pivot_buffer_pct":         map[string]any{"default": 1.5, "type": "float"},
			"breakout_volume_multiple": map[string]any{"default": 1.5, "type": "float"},
			"vcp_lookback":             map[string]any{"default": 5, "type": "int"},
			"history_max_bars":         map[string]any{"default": 1000, "type": "int"},
			"timezone":                 map[string]any{"default": "America/New_York", "type": "str"},
		},
	}
	b, _ := json.Marshal(doc)
	return b
}

func TestResolveDBWins(t *testing.T) {
	ld := params.NewLoader(fakeReader{strategy: "sepa", payload: sepaDoc(3.3)}, "testdata")
	p, doc, err := ld.SEPA(context.Background())
	if err != nil {
		t.Fatalf("SEPA: %v", err)
	}
	if p.RiskPct != 3.3 {
		t.Errorf("risk_pct = %v, want 3.3 (DB tier should win over file)", p.RiskPct)
	}
	if doc.Source != params.OriginDB {
		t.Errorf("source = %q, want db", doc.Source)
	}
}

func TestResolveFileWinsWhenNoDBRow(t *testing.T) {
	// DB has a row for pairs only -> sepa falls through to file (testdata).
	ld := params.NewLoader(fakeReader{strategy: "pairs", payload: nil}, "testdata")
	p, doc, err := ld.SEPA(context.Background())
	if err != nil {
		t.Fatalf("SEPA: %v", err)
	}
	if p.RiskPct != 1.0 {
		t.Errorf("risk_pct = %v, want 1.0 (file baseline)", p.RiskPct)
	}
	if doc.Source != params.OriginFile {
		t.Errorf("source = %q, want file", doc.Source)
	}
}

func TestResolvePartialFileDirFallsBackToBaselinePerStrategy(t *testing.T) {
	// Per-strategy fallback: a dir holding ONLY a tuned sepa.json still serves
	// pairs/sector from the
	// embedded baseline rather than failing.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sepa.json"), sepaDoc(99.9), 0o644); err != nil {
		t.Fatal(err)
	}
	ld := params.NewLoader(nil, dir)
	ctx := context.Background()

	sepa, sdoc, err := ld.SEPA(ctx)
	if err != nil {
		t.Fatalf("SEPA: %v", err)
	}
	if sepa.RiskPct != 99.9 || sdoc.Source != params.OriginFile {
		t.Errorf("sepa = %v src %q, want 99.9/file", sepa.RiskPct, sdoc.Source)
	}

	sector, secdoc, err := ld.SectorRotation(ctx)
	if err != nil {
		t.Fatalf("SectorRotation: %v", err)
	}
	if sector.MomentumLookback != 63 || secdoc.Source != params.OriginBaseline {
		t.Errorf("sector momentum = %d src %q, want 63/baseline", sector.MomentumLookback, secdoc.Source)
	}

	pairs, pdoc, err := ld.Pairs(ctx)
	if err != nil {
		t.Fatalf("Pairs: %v", err)
	}
	if pairs.EntryZ != 2.0 || pdoc.Source != params.OriginBaseline {
		t.Errorf("pairs entry_z = %v src %q, want 2.0/baseline", pairs.EntryZ, pdoc.Source)
	}
}

func TestResolveBaselineWhenNoDBNoDir(t *testing.T) {
	ld := params.NewLoader(nil, "")
	_, doc, err := ld.SEPA(context.Background())
	if err != nil {
		t.Fatalf("SEPA: %v", err)
	}
	if doc.Source != params.OriginBaseline {
		t.Errorf("source = %q, want baseline", doc.Source)
	}
}

// sentinelReader always reports "no active payload" via the error sentinel
// (the DB-backed paramsdb.Reader's no-row form), which Resolve must treat
// identically to (nil, nil): fall through to file/baseline, not error.
type sentinelReader struct{}

func (sentinelReader) ActivePayload(_ context.Context, _ string) (json.RawMessage, error) {
	return nil, params.ErrNoActivePayload
}

func TestResolveSentinelNoActivePayloadFallsThrough(t *testing.T) {
	ld := params.NewLoader(sentinelReader{}, "testdata")
	p, doc, err := ld.SEPA(context.Background())
	if err != nil {
		t.Fatalf("SEPA: %v", err)
	}
	if doc.Source != params.OriginFile {
		t.Errorf("source = %q, want file (sentinel must fall through, not error)", doc.Source)
	}
	if p.RiskPct != 1.0 {
		t.Errorf("risk_pct = %v, want 1.0 (file baseline)", p.RiskPct)
	}
}

func TestResolveDBErrorPropagates(t *testing.T) {
	ld := params.NewLoader(fakeReader{err: errBoom}, "")
	if _, _, err := ld.SEPA(context.Background()); err == nil {
		t.Fatal("expected DB error to propagate")
	}
}

func TestResolveEmptyStrategy(t *testing.T) {
	r := &params.Resolver{}
	if _, err := r.Resolve(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty strategy")
	}
}

func TestResolveUnknownStrategyFromBaseline(t *testing.T) {
	r := &params.Resolver{}
	if _, err := r.Resolve(context.Background(), "does_not_exist"); err == nil {
		t.Fatal("expected error for unknown strategy")
	}
}

// TestParseDocumentRejectsStrategyMismatch asserts the loader rejects a file
// whose declared strategy differs from the requested one.
func TestParseDocumentRejectsStrategyMismatch(t *testing.T) {
	raw := sepaDoc(1.0) // declares strategy "sepa"
	if _, err := params.ParseDocument(raw, "pairs"); err == nil {
		t.Fatal("expected strategy-mismatch error")
	}
}

// TestParseDocumentDisplay checks display decode. An allocation block may still
// be physically present in the JSON, but the Model owns allocation now, so it is
// neither parsed nor exposed (it must not cause a parse error either).
func TestParseDocumentDisplay(t *testing.T) {
	raw := []byte(`{
	  "strategy":"sepa","schema_version":1,
	  "display":{"description":"hello"},
	  "allocation":{"capital_pct":0.4,"active":false},
	  "parameters":{"risk_pct":{"default":1.0,"type":"float"}}
	}`)
	doc, err := params.ParseDocument(raw, "sepa")
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	if doc.Display == nil || doc.Display.Description != "hello" {
		t.Errorf("display = %+v", doc.Display)
	}
}

var errBoom = boomErr("boom")

type boomErr string

func (e boomErr) Error() string { return string(e) }
