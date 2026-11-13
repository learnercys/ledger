package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	ledgerv1 "github.com/learnercys/ledger/gen/ledger/v1"
	"github.com/learnercys/ledger/internal/ledger"
)

// PostTransaction applies a balanced set of postings atomically.
func (s *Store) PostTransaction(ctx context.Context, tenant string,
	req *ledgerv1.PostTransactionRequest) (*ledgerv1.Transaction, error) {
	inputs := make([]ledger.PostingInput, len(req.GetPostings()))
	for i, p := range req.GetPostings() {
		inputs[i] = ledger.PostingInput{AccountID: p.GetAccountId(), Amount: p.GetAmount()}
	}
	if err := ledger.ValidatePostings(inputs); err != nil {
		return nil, err
	}

	resp := &ledgerv1.PostTransactionResponse{}
	err := s.idempotent(ctx, tenant, "PostTransaction", req.GetIdempotencyKey(), withoutKey(req), resp,
		func(tx pgx.Tx) error {
			txn, err := post(ctx, tx, tenant, req.GetDescription(), "", inputs)
			resp.Transaction = txn
			return err
		})
	if err != nil {
		return nil, err
	}
	return resp.GetTransaction(), nil
}

type lockedAccount struct {
	currency      string
	balance       int64
	version       int64
	allowNegative bool
}

// post is the heart of the ledger. Inside one database transaction it:
//
//  1. locks every account involved, always in ID order, so two transfers
//     touching the same accounts can't deadlock each other;
//  2. checks that the postings balance per currency;
//  3. computes each new balance and rejects overdrafts;
//  4. writes the transaction, its postings (with the balance after each one),
//     and the new account balances.
//
// The schema re-checks rules 2 and 3 (deferred trigger and CHECK constraint),
// so a bug here can't commit bad money.
func post(ctx context.Context, tx pgx.Tx, tenant, description, reverses string,
	inputs []ledger.PostingInput) (*ledgerv1.Transaction, error) {
	ids := make([]string, len(inputs))
	for i, p := range inputs {
		ids[i] = p.AccountID
	}
	slices.Sort(ids)

	rows, err := tx.Query(ctx, `
		SELECT id, currency, balance, version, allow_negative FROM accounts
		WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, err
	}
	accounts := make(map[string]*lockedAccount, len(ids))
	for rows.Next() {
		var id string
		a := &lockedAccount{}
		if err := rows.Scan(&id, &a.currency, &a.balance, &a.version, &a.allowNegative); err != nil {
			rows.Close()
			return nil, err
		}
		accounts[id] = a
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if accounts[id] == nil {
			return nil, fmt.Errorf("%w: account %s", ledger.ErrNotFound, id)
		}
	}

	currencyOf := make(map[string]string, len(accounts))
	for id, a := range accounts {
		currencyOf[id] = a.currency
	}
	if err := ledger.CheckBalanced(inputs, currencyOf); err != nil {
		return nil, err
	}

	txn := &ledgerv1.Transaction{Id: newID("txn"), Description: description, ReversesId: reverses}
	var (
		seqs                             []int32
		accountIDs, currencies           []string
		amounts, balancesAfter, versions []int64
	)
	for i, p := range inputs {
		a := accounts[p.AccountID]
		a.balance += p.Amount
		a.version++
		if a.balance < 0 && !a.allowNegative {
			return nil, fmt.Errorf("%w: account %s", ledger.ErrInsufficientFunds, p.AccountID)
		}
		seqs = append(seqs, int32(i))
		accountIDs = append(accountIDs, p.AccountID)
		currencies = append(currencies, a.currency)
		amounts = append(amounts, p.Amount)
		balancesAfter = append(balancesAfter, a.balance)
		versions = append(versions, a.version)
		txn.Postings = append(txn.Postings, &ledgerv1.Posting{
			AccountId: p.AccountID, Amount: p.Amount, Currency: a.currency,
			BalanceAfter: a.balance, AccountVersion: a.version,
		})
	}

	var reversesID *string
	if reverses != "" {
		reversesID = &reverses
	}
	var created time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO transactions (tenant_id, id, description, reverses_id)
		VALUES ($1, $2, $3, $4) RETURNING created_at`,
		tenant, txn.Id, description, reversesID).Scan(&created); err != nil {
		return nil, err
	}
	txn.CreatedAt = timestamppb.New(created)

	if _, err := tx.Exec(ctx, `
		INSERT INTO postings (tenant_id, transaction_id, seq, account_id, currency,
		                      amount, balance_after, account_version)
		SELECT $1, $2, * FROM unnest($3::int[], $4::text[], $5::text[], $6::bigint[], $7::bigint[], $8::bigint[])`,
		tenant, txn.Id, seqs, accountIDs, currencies, amounts, balancesAfter, versions); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE accounts a SET balance = u.balance, version = u.version
		FROM unnest($1::text[], $2::bigint[], $3::bigint[]) AS u(id, balance, version)
		WHERE a.id = u.id`, accountIDs, balancesAfter, versions); err != nil {
		return nil, err
	}
	return txn, nil
}

// GetTransaction returns a transaction with its postings.
func (s *Store) GetTransaction(ctx context.Context, tenant, id string) (*ledgerv1.Transaction, error) {
	var txn *ledgerv1.Transaction
	err := s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		txn, err = loadTransaction(ctx, tx, id)
		return err
	})
	return txn, err
}

func loadTransaction(ctx context.Context, tx pgx.Tx, id string) (*ledgerv1.Transaction, error) {
	var (
		txn      = &ledgerv1.Transaction{Id: id}
		reverses *string
		created  time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT description, reverses_id, created_at FROM transactions WHERE id = $1`, id).
		Scan(&txn.Description, &reverses, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: transaction %s", ledger.ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	if reverses != nil {
		txn.ReversesId = *reverses
	}
	txn.CreatedAt = timestamppb.New(created)

	rows, err := tx.Query(ctx, `
		SELECT account_id, amount, currency, balance_after, account_version
		FROM postings WHERE transaction_id = $1 ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p := &ledgerv1.Posting{}
		if err := rows.Scan(&p.AccountId, &p.Amount, &p.Currency, &p.BalanceAfter, &p.AccountVersion); err != nil {
			return nil, err
		}
		txn.Postings = append(txn.Postings, p)
	}
	return txn, rows.Err()
}

// ReverseTransaction posts the opposite of an earlier transaction. The
// reversed_once unique constraint guarantees two concurrent reversals can't
// both succeed.
func (s *Store) ReverseTransaction(ctx context.Context, tenant string,
	req *ledgerv1.ReverseTransactionRequest) (*ledgerv1.Transaction, error) {
	resp := &ledgerv1.ReverseTransactionResponse{}
	err := s.idempotent(ctx, tenant, "ReverseTransaction", req.GetIdempotencyKey(), withoutKey(req), resp,
		func(tx pgx.Tx) error {
			// Serialise reversals of the same transaction, so a second attempt
			// reliably sees the first and reports "already reversed" (instead
			// of, say, insufficient funds from the first one's postings).
			// Transactions are append-only, so there's no row to SELECT FOR
			// UPDATE; a transaction-scoped advisory lock does the job.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || '/' || $2, 0))`,
				tenant, req.GetTransactionId()); err != nil {
				return err
			}
			var reversed bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM transactions WHERE reverses_id = $1)`,
				req.GetTransactionId()).Scan(&reversed); err != nil {
				return err
			}
			if reversed {
				return fmt.Errorf("%w: %s", ledger.ErrAlreadyReversed, req.GetTransactionId())
			}

			orig, err := loadTransaction(ctx, tx, req.GetTransactionId())
			if err != nil {
				return err
			}
			if orig.GetReversesId() != "" {
				return fmt.Errorf("%w: %s is itself a reversal", ledger.ErrNotReversible, orig.GetId())
			}
			inputs := make([]ledger.PostingInput, len(orig.GetPostings()))
			for i, p := range orig.GetPostings() {
				inputs[i] = ledger.PostingInput{AccountID: p.GetAccountId(), Amount: p.GetAmount()}
			}
			desc := req.GetDescription()
			if desc == "" {
				desc = "Reversal of " + orig.GetId()
			}
			resp.Transaction, err = post(ctx, tx, tenant, desc, orig.GetId(), ledger.Reverse(inputs))
			return err
		})
	if err != nil {
		return nil, err
	}
	return resp.GetTransaction(), nil
}

// ListEntries returns an account's postings, newest first. The page token is
// the last account_version returned.
func (s *Store) ListEntries(ctx context.Context, tenant, accountID string, pageSize int32,
	pageToken string) ([]*ledgerv1.Entry, string, error) {
	limit := pageLimit(pageSize)
	before := int64(1 << 62)
	if pageToken != "" {
		v, err := strconv.ParseInt(pageToken, 10, 64)
		if err != nil {
			return nil, "", fmt.Errorf("%w: bad page_token", ledger.ErrInvalid)
		}
		before = v
	}

	var entries []*ledgerv1.Entry
	err := s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM accounts WHERE id = $1)`,
			accountID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: account %s", ledger.ErrNotFound, accountID)
		}
		rows, err := tx.Query(ctx, `
			SELECT p.transaction_id, p.amount, p.balance_after, p.account_version,
			       t.description, t.created_at
			FROM postings p JOIN transactions t ON t.id = p.transaction_id
			WHERE p.account_id = $1 AND p.account_version < $2
			ORDER BY p.account_version DESC LIMIT $3`, accountID, before, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				e       ledgerv1.Entry
				created time.Time
			)
			if err := rows.Scan(&e.TransactionId, &e.Amount, &e.BalanceAfter, &e.AccountVersion,
				&e.Description, &created); err != nil {
				return err
			}
			e.CreatedAt = timestamppb.New(created)
			entries = append(entries, &e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(entries) > limit {
		entries = entries[:limit]
		next = strconv.FormatInt(entries[limit-1].GetAccountVersion(), 10)
	}
	return entries, next, nil
}
