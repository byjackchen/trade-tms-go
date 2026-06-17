package composition

// store.go is the PG-backed CRUD over tms.compositions + tms.composition_members
// (one Store per pool, mirroring internal/apistore/trade_store.go's construction +
// query style). Money and risk fractions stay float64 — they are DOUBLE PRECISION
// columns, not the 1e-4 fixed-point BIGINT used for the order/position ledger.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store reads and writes Compositions (with their members) in PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore builds a Store over a pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// List returns every Composition (with members), ordered by id.
func (s *Store) List(ctx context.Context) ([]Composition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, description, cash_pct,
		       risk_single_name_pct, risk_concentration_pct, risk_daily_loss_halt_pct,
		       risk_max_gross_pct, risk_max_positions, version
		  FROM tms.compositions
		 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Composition
	for rows.Next() {
		m, err := scanComposition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Attach members to each composition (one query, then bucket by composition_id).
	if err := s.loadMembers(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Get returns the Composition with the given id, or (nil, ErrNotFound) when absent.
func (s *Store) Get(ctx context.Context, id string) (*Composition, error) {
	m, err := scanComposition(s.pool.QueryRow(ctx, `
		SELECT id, name, description, cash_pct,
		       risk_single_name_pct, risk_concentration_pct, risk_daily_loss_halt_pct,
		       risk_max_gross_pct, risk_max_positions, version
		  FROM tms.compositions
		 WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	mems, err := s.queryMembers(ctx, s.pool, id)
	if err != nil {
		return nil, err
	}
	m.Members = mems
	return &m, nil
}

// Create inserts a new Composition and its members in one transaction. It rejects
// an invalid Composition (Validate) before touching the DB.
func (s *Store) Create(ctx context.Context, m Composition) error {
	if err := m.Validate(); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO tms.compositions
		    (id, name, description, cash_pct,
		     risk_single_name_pct, risk_concentration_pct, risk_daily_loss_halt_pct,
		     risk_max_gross_pct, risk_max_positions, version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		m.ID, m.Name, m.Description, m.CashPct,
		m.Risk.SingleNamePct, m.Risk.ConcentrationPct, m.Risk.DailyLossHaltPct,
		m.Risk.MaxGrossPct, m.Risk.MaxPositions, compositionVersion(m.Version)); err != nil {
		return err
	}
	if err := upsertMembers(ctx, tx, m.ID, m.Members); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Update overwrites an existing Composition's columns and replaces its member set
// in one transaction (delete-then-insert). It returns ErrNotFound if id is absent.
func (s *Store) Update(ctx context.Context, m Composition) error {
	if err := m.Validate(); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE tms.compositions
		   SET name = $2, description = $3, cash_pct = $4,
		       risk_single_name_pct = $5, risk_concentration_pct = $6, risk_daily_loss_halt_pct = $7,
		       risk_max_gross_pct = $8, risk_max_positions = $9, version = $10
		 WHERE id = $1`,
		m.ID, m.Name, m.Description, m.CashPct,
		m.Risk.SingleNamePct, m.Risk.ConcentrationPct, m.Risk.DailyLossHaltPct,
		m.Risk.MaxGrossPct, m.Risk.MaxPositions, compositionVersion(m.Version))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	// Replace the member set: clear then re-insert (handles add/remove/edit).
	if _, err := tx.Exec(ctx, `DELETE FROM tms.composition_members WHERE composition_id = $1`, m.ID); err != nil {
		return err
	}
	if err := upsertMembers(ctx, tx, m.ID, m.Members); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Delete removes a Composition; members cascade via the FK. It returns ErrNotFound
// if id is absent.
func (s *Store) Delete(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM tms.compositions WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- helpers ---

// scanRow is the read surface shared by pool.QueryRow and rows.Next scans.
type scanRow interface {
	Scan(dest ...any) error
}

// scanComposition reads a compositions row into a Composition (without its members).
func scanComposition(row scanRow) (Composition, error) {
	var m Composition
	err := row.Scan(&m.ID, &m.Name, &m.Description, &m.CashPct,
		&m.Risk.SingleNamePct, &m.Risk.ConcentrationPct, &m.Risk.DailyLossHaltPct,
		&m.Risk.MaxGrossPct, &m.Risk.MaxPositions, &m.Version)
	return m, err
}

// loadMembers fills Members on each composition in ms with one bucketed query.
func (s *Store) loadMembers(ctx context.Context, ms []Composition) error {
	if len(ms) == 0 {
		return nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT composition_id, strategy_id, weight, active, param_set_id
		  FROM tms.composition_members
		 ORDER BY composition_id, strategy_id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	byComposition := make(map[string][]Member, len(ms))
	for rows.Next() {
		var id string
		var mem Member
		if err := rows.Scan(&id, &mem.StrategyID, &mem.Weight, &mem.Active, &mem.ParamSetID); err != nil {
			return err
		}
		byComposition[id] = append(byComposition[id], mem)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range ms {
		ms[i].Members = byComposition[ms[i].ID]
	}
	return nil
}

// queryMembers returns a single composition's members, ordered by strategy_id.
func (s *Store) queryMembers(ctx context.Context, q querier, compositionID string) ([]Member, error) {
	rows, err := q.Query(ctx, `
		SELECT strategy_id, weight, active, param_set_id
		  FROM tms.composition_members
		 WHERE composition_id = $1
		 ORDER BY strategy_id`, compositionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var mem Member
		if err := rows.Scan(&mem.StrategyID, &mem.Weight, &mem.Active, &mem.ParamSetID); err != nil {
			return nil, err
		}
		out = append(out, mem)
	}
	return out, rows.Err()
}

// upsertMembers inserts the given members for compositionID. Callers run it inside
// a transaction after clearing the existing rows (Update) or fresh insert (Create).
func upsertMembers(ctx context.Context, tx pgx.Tx, compositionID string, members []Member) error {
	for _, mem := range members {
		if _, err := tx.Exec(ctx, `
			INSERT INTO tms.composition_members (composition_id, strategy_id, weight, active, param_set_id)
			VALUES ($1,$2,$3,$4,$5)`,
			compositionID, mem.StrategyID, mem.Weight, mem.Active, mem.ParamSetID); err != nil {
			return fmt.Errorf("composition %q member %q: %w", compositionID, mem.StrategyID, err)
		}
	}
	return nil
}

// compositionVersion defaults an unset (zero) version to 1, matching the column default.
func compositionVersion(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

// querier is the subset of pgx query surface shared by *pgxpool.Pool and pgx.Tx.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}
