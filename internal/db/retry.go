package db

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"syscall"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
)

// BackoffPolicy describes a capped exponential backoff with optional
// jitter, used by Retry. The zero value is invalid; start from
// DefaultBackoff and override fields as needed.
type BackoffPolicy struct {
	// MaxAttempts is the total number of attempts including the first
	// (so MaxAttempts=4 means up to 3 retries). Must be >= 1.
	MaxAttempts int
	// BaseDelay is the wait after the first failed attempt.
	BaseDelay time.Duration
	// MaxDelay caps the per-wait delay (pre-jitter). <= 0 means no cap.
	MaxDelay time.Duration
	// Multiplier scales the delay after each failure (>= 1 expected;
	// values < 1 are treated as 1, i.e. constant backoff).
	Multiplier float64
	// JitterFrac in [0, 1] randomizes each delay by ±JitterFrac*delay,
	// de-synchronizing competing retriers. 0 = deterministic (testable).
	JitterFrac float64
	// IsTransient classifies whether an error is worth retrying.
	// nil means the package default IsTransient (Postgres/network aware).
	IsTransient func(error) bool

	// sleep is a test seam; nil means context-aware real sleeping.
	sleep func(ctx context.Context, d time.Duration) error
}

// DefaultBackoff is the recommended policy for transient database errors:
// 4 attempts with 100ms → 200ms → 400ms waits (capped at 2s), ±20% jitter.
func DefaultBackoff() BackoffPolicy {
	return BackoffPolicy{
		MaxAttempts: 4,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    2 * time.Second,
		Multiplier:  2,
		JitterFrac:  0.2,
	}
}

// Delay returns the wait after the attempt-th failure (attempt is 1-based):
// BaseDelay * Multiplier^(attempt-1), capped at MaxDelay, then jittered by
// ±JitterFrac. Never negative.
func (p BackoffPolicy) Delay(attempt int) time.Duration {
	if attempt < 1 || p.BaseDelay <= 0 {
		return 0
	}
	mult := p.Multiplier
	if mult < 1 {
		mult = 1
	}
	d := float64(p.BaseDelay)
	for i := 1; i < attempt; i++ {
		d *= mult
		if p.MaxDelay > 0 && d >= float64(p.MaxDelay) {
			break // already at cap; avoid float overflow on large attempt
		}
	}
	if p.MaxDelay > 0 && d > float64(p.MaxDelay) {
		d = float64(p.MaxDelay)
	}
	if p.JitterFrac > 0 {
		f := min(p.JitterFrac, 1)
		d *= 1 + f*(2*rand.Float64()-1)
	}
	if d < 0 {
		return 0
	}
	return time.Duration(d)
}

// Retry runs op, retrying transient failures per the policy. Semantics:
//   - op returning nil ends the loop with success;
//   - a non-transient error (per policy.IsTransient) is returned as-is,
//     immediately — callers keep full errors.Is/As fidelity;
//   - context cancellation aborts promptly (including mid-backoff) and is
//     reported via errors.Join with the last transient error, so both
//     errors.Is(err, context.Canceled) and the cause remain matchable;
//   - after MaxAttempts transient failures the last error is returned
//     wrapped with the attempt count (errors.Is/As still reach it).
func Retry(ctx context.Context, p BackoffPolicy, op func(context.Context) error) error {
	if p.MaxAttempts < 1 {
		return fmt.Errorf("db: retry: MaxAttempts must be >= 1, got %d", p.MaxAttempts)
	}
	classify := p.IsTransient
	if classify == nil {
		classify = IsTransient
	}
	sleep := p.sleep
	if sleep == nil {
		sleep = sleepCtx
	}

	var lastErr error
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return errors.Join(err, lastErr)
			}
			return err
		}
		err := op(ctx)
		if err == nil {
			return nil
		}
		if !classify(err) {
			return err
		}
		lastErr = err
		if attempt >= p.MaxAttempts {
			return fmt.Errorf("db: retry: gave up after %d attempts: %w", p.MaxAttempts, lastErr)
		}
		if serr := sleep(ctx, p.Delay(attempt)); serr != nil {
			return errors.Join(serr, lastErr)
		}
	}
}

// sleepCtx waits d or until ctx is done, returning ctx.Err() in the latter
// case. Shutdown cancels in-flight backoff instead of blocking on a timer.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// IsTransient reports whether err looks like a transient database/network
// failure worth retrying. Deliberately conservative: anything ambiguous is
// non-transient, because retrying a non-idempotent statement that may have
// reached the server is worse than failing loudly.
//
// Transient:
//   - Postgres SQLSTATE class 08 (connection exception), 40001
//     (serialization_failure), 40P01 (deadlock_detected), 57P01/57P02/57P03
//     (admin/crash shutdown, cannot_connect_now), 53300 (too_many_connections);
//   - pgconn.SafeToRetry errors (pgx guarantees nothing was sent);
//   - pgconn.ConnectError (dial/startup failed; nothing executed);
//   - net.Error, ECONNREFUSED/ECONNRESET/EPIPE, io.EOF/io.ErrUnexpectedEOF.
//
// Never transient: nil, context.Canceled, context.DeadlineExceeded.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgerrcode.IsConnectionException(pgErr.Code):
			return true
		}
		switch pgErr.Code {
		case pgerrcode.SerializationFailure,
			pgerrcode.DeadlockDetected,
			pgerrcode.AdminShutdown,
			pgerrcode.CrashShutdown,
			pgerrcode.CannotConnectNow,
			pgerrcode.TooManyConnections:
			return true
		}
		return false // a definite server error (syntax, constraint, ...) — do not retry
	}

	if pgconn.SafeToRetry(err) {
		return true
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return false
}
