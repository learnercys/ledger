package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	ledgerv1 "github.com/learnercys/ledger/gen/ledger/v1"
	"github.com/learnercys/ledger/internal/ledger"
)

func newID(prefix string) string {
	return prefix + "_" + uuid.Must(uuid.NewV7()).String()
}

// CreateAccount opens an account. Retries with the same idempotency key
// return the same account.
func (s *Store) CreateAccount(ctx context.Context, tenant string,
	req *ledgerv1.CreateAccountRequest) (*ledgerv1.Account, error) {
	if err := ledger.ValidateName(req.GetName()); err != nil {
		return nil, err
	}
	if err := ledger.ValidateCurrency(req.GetCurrency()); err != nil {
		return nil, err
	}

	resp := &ledgerv1.CreateAccountResponse{}
	err := s.idempotent(ctx, tenant, "CreateAccount", req.GetIdempotencyKey(), withoutKey(req), resp,
		func(tx pgx.Tx) error {
			var created time.Time
			id := newID("acct")
			if err := tx.QueryRow(ctx, `
				INSERT INTO accounts (tenant_id, id, name, currency, allow_negative)
				VALUES ($1, $2, $3, $4, $5)
				RETURNING created_at`,
				tenant, id, req.GetName(), req.GetCurrency(), req.GetAllowNegative()).Scan(&created); err != nil {
				return err
			}
			resp.Account = &ledgerv1.Account{
				Id: id, Name: req.GetName(), Currency: req.GetCurrency(),
				AllowNegative: req.GetAllowNegative(), CreatedAt: timestamppb.New(created),
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return resp.GetAccount(), nil
}

const accountColumns = `id, name, currency, allow_negative, balance, version, created_at`

func scanAccount(row pgx.Row) (*ledgerv1.Account, error) {
	var (
		a       ledgerv1.Account
		created time.Time
	)
	if err := row.Scan(&a.Id, &a.Name, &a.Currency, &a.AllowNegative, &a.Balance, &a.Version, &created); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ledger.ErrNotFound
		}
		return nil, err
	}
	a.CreatedAt = timestamppb.New(created)
	return &a, nil
}

// GetAccount returns one account with its current balance.
func (s *Store) GetAccount(ctx context.Context, tenant, id string) (*ledgerv1.Account, error) {
	var acct *ledgerv1.Account
	err := s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		acct, err = scanAccount(tx.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id = $1`, id))
		return err
	})
	return acct, err
}

// ListAccounts pages through accounts in creation order. IDs are UUIDv7, so
// ordering by ID is ordering by time, and the last ID is the page token.
func (s *Store) ListAccounts(ctx context.Context, tenant string, pageSize int32,
	after string) ([]*ledgerv1.Account, string, error) {
	limit := pageLimit(pageSize)
	var accounts []*ledgerv1.Account
	err := s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+accountColumns+` FROM accounts
			WHERE id > $1 ORDER BY id LIMIT $2`, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanAccount(rows)
			if err != nil {
				return err
			}
			accounts = append(accounts, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(accounts) > limit {
		accounts = accounts[:limit]
		next = accounts[limit-1].GetId()
	}
	return accounts, next, nil
}

func pageLimit(size int32) int {
	switch {
	case size <= 0:
		return 50
	case size > 200:
		return 200
	}
	return int(size)
}

// withoutKey clears the idempotency key before hashing, so the hash covers
// only what the request asks for.
func withoutKey[T proto.Message](req T) T {
	c := proto.Clone(req)
	r := c.ProtoReflect()
	if f := r.Descriptor().Fields().ByName("idempotency_key"); f != nil {
		r.Clear(f)
	}
	return c.(T)
}
