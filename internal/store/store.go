// Package store is the Postgres implementation of the ledger.
package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/learnercys/ledger/internal/ledger"
)

// Store is a tenant-aware Postgres ledger.
type Store struct {
	pool *pgxpool.Pool
}

// New connects to Postgres and waits until it answers.
func New(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	// A span per query, under the RPC that issued it.
	cfg.ConnConfig.Tracer = otelpgx.NewTracer()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err = pool.Ping(ctx); err == nil {
			return &Store{pool: pool}, nil
		}
		if time.Now().After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("ping postgres: %w", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Close releases all connections.
func (s *Store) Close() { s.pool.Close() }

// Ping checks the database.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// maxAttempts bounds retries of transactions that lost a deadlock or
// serialization race. Lock ordering makes deadlocks unlikely, not impossible.
const maxAttempts = 5

// inTenant runs fn in a transaction scoped to one tenant. Row-level security
// uses app.tenant_id, so every query inside fn only sees that tenant's rows,
// even one that forgets a WHERE tenant_id clause.
func (s *Store) inTenant(ctx context.Context, tenant string, fn func(pgx.Tx) error) error {
	if tenant == "" {
		return errors.New("store: empty tenant")
	}
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
				return err
			}
			return fn(tx)
		})
		if !retryable(err) {
			return translate(err)
		}
		time.Sleep(time.Duration(attempt*attempt) * 5 * time.Millisecond)
	}
	return translate(err)
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		(pgErr.Code == "40P01" || pgErr.Code == "40001") // deadlock, serialization failure
}

// translate turns database constraint errors into ledger errors. The
// application checks these rules first; these are the backstops.
func translate(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch {
	case pgErr.ConstraintName == "balance_non_negative":
		return ledger.ErrInsufficientFunds
	case pgErr.ConstraintName == "reversed_once":
		return ledger.ErrAlreadyReversed
	case pgErr.Code == "23514": // check_violation, e.g. unbalanced transaction trigger
		return fmt.Errorf("%w: %s", ledger.ErrInvalid, pgErr.Message)
	}
	return err
}

// idempotent runs fn at most once per (tenant, key). The key row is inserted
// in the same transaction as fn's writes:
//
//   - first request: the insert succeeds, fn runs, the response is stored, and
//     everything commits together;
//   - a retry after commit: the insert conflicts, the stored response is
//     returned, and fn never runs;
//   - a concurrent retry: its insert waits on the first transaction's row lock,
//     then conflicts and returns the stored response;
//   - the same key with a different request: ErrIdempotencyMismatch.
//
// If fn fails, the whole transaction (key row included) rolls back, so the
// client can retry once the cause is fixed. Only successes are remembered.
func (s *Store) idempotent(ctx context.Context, tenant, method, key string,
	req, resp proto.Message, fn func(pgx.Tx) error) error {
	if err := ledger.ValidateIdempotencyKey(key); err != nil {
		return err
	}
	hash, err := requestHash(method, req)
	if err != nil {
		return err
	}
	return s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (tenant_id, key, method, request_hash)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING`, tenant, key, method, hash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var storedHash, stored []byte
			if err := tx.QueryRow(ctx, `
				SELECT request_hash, response FROM idempotency_keys WHERE key = $1`, key).
				Scan(&storedHash, &stored); err != nil {
				return err
			}
			if string(storedHash) != string(hash) {
				return ledger.ErrIdempotencyMismatch
			}
			return proto.Unmarshal(stored, resp)
		}

		if err := fn(tx); err != nil {
			return err
		}
		out, err := proto.MarshalOptions{Deterministic: true}.Marshal(resp)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE idempotency_keys SET response = $1 WHERE key = $2`, out, key)
		return err
	})
}

// requestHash fingerprints a request so a reused key can be detected. The
// method name is included so the same key can't replay across endpoints.
func requestHash(method string, req proto.Message) ([]byte, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write(b)
	return h.Sum(nil), nil
}
