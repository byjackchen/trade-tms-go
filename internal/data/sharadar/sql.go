package sharadar

// sql.go generates the staging DDL and merge statements. One session temp
// staging table per dataset; the merge keeps "new rows win" parity with the
// Python writers' drop_duplicates(keep="last") via DISTINCT ON ordered by
// the staging sequence descending (see doc.go for the design rationale).

import (
	"fmt"
	"strings"
)

// stagingPlan is everything the loader needs for one target table.
type stagingPlan struct {
	staging   string   // temp table name
	columns   []string // copy columns (without seq; the loader prepends seq)
	createSQL string
	upsertSQL string
	// mergeCountSQL wraps upsertSQL to report (total affected, net-new
	// inserts). The xmax = 0 predicate distinguishes fresh inserts from
	// conflict updates, giving the Python writers' `added = len(merged) -
	// len(existing)` net-new-keys semantics (spec §6 step 3) — revised rows
	// are applied but not counted, exactly like the original.
	mergeCountSQL string
}

func quoteJoin(cols []string) string { return strings.Join(cols, ", ") }

func prefixJoin(prefix string, cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = prefix + c
	}
	return strings.Join(out, ", ")
}

func setClause(cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = fmt.Sprintf("%s = EXCLUDED.%s", c, c)
	}
	return strings.Join(out, ", ")
}

// newStagingPlan builds the plan for a target table.
//
//	target   — fully qualified target table (e.g. "tms.bars_daily")
//	staging  — temp table name
//	colDefs  — "name TYPE" definitions in copy order (seq added here)
//	keyCols  — conflict/dedup key columns
//	keysOnly — true when the row is entirely key columns (DO NOTHING merge)
func newStagingPlan(target, staging string, colDefs []string, keyCols []string, keysOnly bool) stagingPlan {
	cols := make([]string, len(colDefs))
	for i, d := range colDefs {
		cols[i] = strings.Fields(d)[0]
	}

	createSQL := fmt.Sprintf(
		"CREATE TEMP TABLE IF NOT EXISTS %s (seq BIGINT NOT NULL, %s)",
		staging, strings.Join(colDefs, ", "),
	)

	// Last-wins dedup inside the staged batch, then merge. The writers sort
	// and dedup by the key with keep="last" (spec §6); seq preserves staged
	// order, so ORDER BY key, seq DESC + DISTINCT ON picks the same survivor.
	dedup := fmt.Sprintf(
		"SELECT DISTINCT ON (%s) %s FROM %s ORDER BY %s, seq DESC",
		quoteJoin(keyCols), quoteJoin(cols), staging, quoteJoin(keyCols),
	)

	var conflict string
	if keysOnly {
		conflict = fmt.Sprintf("ON CONFLICT (%s) DO NOTHING", quoteJoin(keyCols))
	} else {
		nonKey := make([]string, 0, len(cols))
		keySet := make(map[string]struct{}, len(keyCols))
		for _, k := range keyCols {
			keySet[k] = struct{}{}
		}
		for _, c := range cols {
			if _, isKey := keySet[c]; !isKey {
				nonKey = append(nonKey, c)
			}
		}
		conflict = fmt.Sprintf("ON CONFLICT (%s) DO UPDATE SET %s", quoteJoin(keyCols), setClause(nonKey))
	}

	upsertSQL := fmt.Sprintf(
		"INSERT INTO %s (%s) SELECT %s FROM (%s) AS staged %s",
		target, quoteJoin(cols), prefixJoin("staged.", cols), dedup, conflict,
	)

	mergeCountSQL := fmt.Sprintf(
		"WITH merged AS (%s RETURNING (xmax = 0) AS inserted) "+
			"SELECT count(*)::bigint, count(*) FILTER (WHERE inserted)::bigint FROM merged",
		upsertSQL,
	)

	return stagingPlan{
		staging: staging, columns: cols,
		createSQL: createSQL, upsertSQL: upsertSQL, mergeCountSQL: mergeCountSQL,
	}
}

func barsPlan() stagingPlan {
	return newStagingPlan(
		"tms.bars_daily", "_stage_bars_daily",
		[]string{
			"ticker TEXT NOT NULL", "ts TIMESTAMPTZ NOT NULL", "source TEXT NOT NULL",
			"open BIGINT", "high BIGINT", "low BIGINT", "close BIGINT", "volume BIGINT",
			"close_adj BIGINT", "close_unadj BIGINT", "dividends BIGINT", "last_updated DATE",
		},
		[]string{"ticker", "ts", "source"},
		false,
	)
}

func tickersPlan() stagingPlan {
	return newStagingPlan(
		"tms.tickers", "_stage_tickers",
		[]string{
			"ticker TEXT NOT NULL", "name TEXT", "exchange TEXT", "is_delisted BOOLEAN NOT NULL",
			"category TEXT", "sector TEXT", "industry TEXT", "table_name TEXT NOT NULL",
			"first_price_date DATE", "last_price_date DATE", "delist_date DATE",
		},
		[]string{"ticker"},
		false,
	)
}

func sf1Plan() stagingPlan {
	defs := []string{
		"ticker TEXT NOT NULL", "dimension TEXT NOT NULL", "calendardate DATE",
		"datekey DATE NOT NULL", "reportperiod DATE", "fiscalperiod TEXT", "lastupdated DATE",
	}
	for _, m := range sf1MetricColumns {
		defs = append(defs, m+" DOUBLE PRECISION")
	}
	return newStagingPlan(
		"tms.fundamentals_sf1", "_stage_fundamentals_sf1",
		defs,
		[]string{"ticker", "datekey", "dimension"},
		false,
	)
}

func eventsPlan() stagingPlan {
	return newStagingPlan(
		"tms.events", "_stage_events",
		[]string{"ticker TEXT NOT NULL", "event_date DATE NOT NULL", "eventcodes TEXT NOT NULL"},
		[]string{"ticker", "event_date", "eventcodes"},
		true, // the whole row is the key: revisions are impossible, DO NOTHING
	)
}

// tickersDeleteMissingSQL implements the Python writer's full-overwrite
// semantics for TICKERS (spec §2.5): rows absent from the source file are
// removed. Only executed when no --tickers filter narrows the import.
const tickersDeleteMissingSQL = "DELETE FROM tms.tickers t WHERE NOT EXISTS (SELECT 1 FROM _stage_tickers s WHERE s.ticker = t.ticker)"

// datasetSyncSQL mirrors CacheMeta.record_sync (spec §5): last_sync is the
// wall-clock sync time, row_count is the rows written by this run
// (bootstrap semantics — the importer is a bulk load, not an incremental
// catchup).
//
// Note (data-freshness): last_sync is an OPERATION timestamp, not a data
// frontier — after a bulk parquet import it equals "now" even though the
// newest bar may be days old. The catchup window is therefore driven by the
// actual data frontier (frontierSQL below / DataFrontier), never by
// last_sync. last_sync is retained only for the SF1/EVENTS lastupdated.gte
// incremental filter (spec §6.6) and operational observability.
const datasetSyncSQL = `
INSERT INTO tms.dataset_sync (dataset, last_sync, row_count)
VALUES ($1, now(), $2)
ON CONFLICT (dataset) DO UPDATE SET last_sync = EXCLUDED.last_sync, row_count = EXCLUDED.row_count`

// frontierSQL maps each dated dataset to the query returning the maximum
// stored DATA date — the "how current is the data" source of truth shared
// by EnsureFresh's catchup window and the go-live preflight (task 2).
//
//   - SEP/SFP: max(ts) of tms.bars_daily filtered by source. ts is the
//     trading date at UTC midnight, so its UTC calendar date IS the bar
//     date (no timezone shift). The (source, ts) prefix of the primary key
//     makes this an index-only max.
//   - SF1: max(datekey) — the filing/point-in-time key (spec §2.3).
//   - EVENTS: max(event_date).
//
// TICKERS has no date column (full-overwrite universe) and is intentionally
// absent: DataFrontier returns ok=false for it.
var frontierSQL = map[string]string{
	DatasetSEP:    "SELECT max(ts) FROM tms.bars_daily WHERE source = 'SEP'",
	DatasetSFP:    "SELECT max(ts) FROM tms.bars_daily WHERE source = 'SFP'",
	DatasetSF1:    "SELECT max(datekey)::timestamptz FROM tms.fundamentals_sf1",
	DatasetEvents: "SELECT max(event_date)::timestamptz FROM tms.events",
}
