package apistore

// accounts_store.go is the CRUD write path for the first-class, USER-MANAGED
// accounts registry (tms.accounts). Accounts are no longer derived from .env at
// node start — they are created/edited/deleted from the UI and the trade-run node
// binds one by env-default (or an explicit id). The account id is an OPAQUE
// surrogate ("acct_<uuid>") so venue/env/broker_acc_id stay freely editable
// without rewriting FK history.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/byjackchen/trade-tms-go/internal/api"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// accountCols is the column list every account read returns, in scanAccount order.
const accountCols = `id, venue, env, broker_acc_id, label, is_default, notes, created_at, updated_at`

// scanAccount maps one tms.accounts row to the wire shape, deriving Kind from env
// (env stays the source of truth) and formatting the timestamps RFC3339.
func scanAccount(row pgx.Row) (api.TradeAccountInfo, error) {
	var a api.TradeAccountInfo
	var created, updated time.Time
	if err := row.Scan(&a.ID, &a.Venue, &a.Env, &a.BrokerAccID, &a.Label,
		&a.IsDefault, &a.Notes, &created, &updated); err != nil {
		return api.TradeAccountInfo{}, err
	}
	a.Kind = domain.AccountKind(domain.BrokerEnv(a.Env))
	a.CreatedAt = created.UTC().Format(time.RFC3339)
	a.UpdatedAt = updated.UTC().Format(time.RFC3339)
	return a, nil
}

// validateWrite normalises + validates a create/update body.
func validateWrite(req api.AccountWriteRequest) (venue, env string, err error) {
	venue = strings.TrimSpace(req.Venue)
	env = strings.TrimSpace(req.Env)
	if venue == "" {
		return "", "", fmt.Errorf("%w: venue required", api.ErrInvalidAccount)
	}
	if !domain.BrokerEnv(env).IsValid() {
		return "", "", fmt.Errorf("%w: env %q invalid (want paper|real)", api.ErrInvalidAccount, env)
	}
	if req.BrokerAccID < 0 {
		return "", "", fmt.Errorf("%w: broker_acc_id must be >= 0", api.ErrInvalidAccount)
	}
	return venue, env, nil
}

// clearDefault unsets is_default on every OTHER account in the (venue, env) group
// so the partial-unique accounts_one_default_per_env index is never violated. It
// first takes a transaction-scoped advisory lock keyed on (venue, env) so that two
// concurrent default-setters for the SAME group serialize — otherwise both could
// clear the other's default in their own snapshot and then collide on the unique
// index (a 23505 surfaced as a confusing 500).
func clearDefault(ctx context.Context, tx pgx.Tx, venue, env, exceptID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1 || ':' || $2))`, venue, env); err != nil {
		return err
	}
	_, err := tx.Exec(ctx,
		`UPDATE tms.accounts SET is_default = false, updated_at = now()
		  WHERE venue = $1 AND env = $2 AND is_default AND id <> $3`,
		venue, env, exceptID)
	return err
}

// CreateAccount inserts a new user-managed account with a surrogate id.
func (s *TradeStore) CreateAccount(ctx context.Context, req api.AccountWriteRequest) (api.TradeAccountInfo, error) {
	venue, env, err := validateWrite(req)
	if err != nil {
		return api.TradeAccountInfo{}, err
	}
	id := "acct_" + uuid.NewString()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return api.TradeAccountInfo{}, err
	}
	defer tx.Rollback(ctx)

	if req.IsDefault {
		if err := clearDefault(ctx, tx, venue, env, id); err != nil {
			return api.TradeAccountInfo{}, err
		}
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO tms.accounts (id, venue, env, broker_acc_id, label, is_default, notes)
		      VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING `+accountCols,
		id, venue, env, req.BrokerAccID, req.Label, req.IsDefault, req.Notes)
	a, err := scanAccount(row)
	if err != nil {
		return api.TradeAccountInfo{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return api.TradeAccountInfo{}, err
	}
	return a, nil
}

// UpdateAccount applies a PARTIAL patch (only the non-nil fields) to an existing
// account. The current row is read FOR UPDATE inside the tx and the patch is merged
// onto it, so an omitted field is preserved (never blanked) and a concurrent writer
// can't clobber the merge. Returns ErrAccountNotFound when id is unknown.
func (s *TradeStore) UpdateAccount(ctx context.Context, id string, patch api.AccountPatchRequest) (api.TradeAccountInfo, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return api.TradeAccountInfo{}, err
	}
	defer tx.Rollback(ctx)

	cur, err := scanAccount(tx.QueryRow(ctx,
		`SELECT `+accountCols+` FROM tms.accounts WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return api.TradeAccountInfo{}, fmt.Errorf("%w: %q", api.ErrAccountNotFound, id)
	}
	if err != nil {
		return api.TradeAccountInfo{}, err
	}

	// Merge: only the fields the patch actually sent.
	venue, env, brokerAccID := cur.Venue, cur.Env, cur.BrokerAccID
	label, notes, isDefault := cur.Label, cur.Notes, cur.IsDefault
	if patch.Venue != nil {
		venue = strings.TrimSpace(*patch.Venue)
	}
	if patch.Env != nil {
		env = strings.TrimSpace(*patch.Env)
	}
	if patch.BrokerAccID != nil {
		brokerAccID = *patch.BrokerAccID
	}
	if patch.Label != nil {
		label = *patch.Label
	}
	if patch.Notes != nil {
		notes = *patch.Notes
	}
	if patch.IsDefault != nil {
		isDefault = *patch.IsDefault
	}

	if venue == "" {
		return api.TradeAccountInfo{}, fmt.Errorf("%w: venue required", api.ErrInvalidAccount)
	}
	if !domain.BrokerEnv(env).IsValid() {
		return api.TradeAccountInfo{}, fmt.Errorf("%w: env %q invalid (want paper|real)", api.ErrInvalidAccount, env)
	}
	if brokerAccID < 0 {
		return api.TradeAccountInfo{}, fmt.Errorf("%w: broker_acc_id must be >= 0", api.ErrInvalidAccount)
	}

	if isDefault {
		if err := clearDefault(ctx, tx, venue, env, id); err != nil {
			return api.TradeAccountInfo{}, err
		}
	}
	a, err := scanAccount(tx.QueryRow(ctx,
		`UPDATE tms.accounts
		    SET venue = $2, env = $3, broker_acc_id = $4, label = $5,
		        is_default = $6, notes = $7, updated_at = now()
		  WHERE id = $1
		 RETURNING `+accountCols,
		id, venue, env, brokerAccID, label, isDefault, notes))
	if err != nil {
		return api.TradeAccountInfo{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return api.TradeAccountInfo{}, err
	}
	return a, nil
}

// DeleteAccount hard-deletes an account. The account_id FKs are ON DELETE RESTRICT,
// so a referenced account raises a 23503 foreign-key violation, which we surface as
// api.ErrAccountInUse (the handler maps it to 409).
func (s *TradeStore) DeleteAccount(ctx context.Context, id string) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM tms.accounts WHERE id = $1`, id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return api.ErrAccountInUse
		}
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w: %q", api.ErrAccountNotFound, id)
	}
	return nil
}

// GetAccount returns one account by id, or api.ErrAccountNotFound when absent.
func (s *TradeStore) GetAccount(ctx context.Context, id string) (api.TradeAccountInfo, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+accountCols+` FROM tms.accounts WHERE id = $1`, id)
	a, err := scanAccount(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.TradeAccountInfo{}, fmt.Errorf("%w: %q", api.ErrAccountNotFound, id)
	}
	return a, err
}

// DefaultAccount returns THE default account for (venue, env) — what a
// `tms trade run --env paper|real` binds when no explicit account id is given.
// Returns pgx.ErrNoRows when no default is set for that env.
func (s *TradeStore) DefaultAccount(ctx context.Context, venue string, env domain.BrokerEnv) (api.TradeAccountInfo, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+accountCols+` FROM tms.accounts
		  WHERE venue = $1 AND env = $2 AND is_default`,
		venue, string(env))
	return scanAccount(row)
}
