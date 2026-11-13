//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	ledgerv1 "github.com/learnercys/ledger/gen/ledger/v1"
	"github.com/learnercys/ledger/internal/ledger"
)

// Run with `make test-integration`. Tests connect as ledger_app, the same
// unprivileged role the service uses, so row-level security is in force.
func testStore(t testing.TB) *Store {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://ledger_app:ledger_app@localhost:55433/ledger?sslmode=disable"
	}
	s, err := New(context.Background(), url)
	if err != nil {
		t.Skipf("postgres not available: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

var tenantSeq atomic.Int64

// newTenant gives every test its own tenant, so tests can run in parallel
// against one database without seeing each other's data.
func newTenant() string {
	return fmt.Sprintf("t%d_%d", time.Now().UnixNano(), tenantSeq.Add(1))
}

var keySeq atomic.Int64

func key() string { return fmt.Sprintf("k%d", keySeq.Add(1)) }

func mustAccount(t testing.TB, s *Store, tenant, currency string, allowNegative bool) string {
	t.Helper()
	a, err := s.CreateAccount(context.Background(), tenant, &ledgerv1.CreateAccountRequest{
		IdempotencyKey: key(), Name: "acct", Currency: currency, AllowNegative: allowNegative,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a.GetId()
}

func transfer(s *Store, tenant, from, to string, amount int64, idemKey string) (*ledgerv1.Transaction, error) {
	return s.PostTransaction(context.Background(), tenant, &ledgerv1.PostTransactionRequest{
		IdempotencyKey: idemKey,
		Postings: []*ledgerv1.Posting{
			{AccountId: from, Amount: -amount},
			{AccountId: to, Amount: amount},
		},
	})
}

func balance(t testing.TB, s *Store, tenant, id string) int64 {
	t.Helper()
	a, err := s.GetAccount(context.Background(), tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	return a.GetBalance()
}

func TestTransferAndStatement(t *testing.T) {
	t.Parallel()
	s, tenant := testStore(t), newTenant()
	world := mustAccount(t, s, tenant, "USD", true)
	alice := mustAccount(t, s, tenant, "USD", false)
	bob := mustAccount(t, s, tenant, "USD", false)

	if _, err := transfer(s, tenant, world, alice, 10_000, key()); err != nil {
		t.Fatal(err)
	}
	txn, err := transfer(s, tenant, alice, bob, 2_500, key())
	if err != nil {
		t.Fatal(err)
	}

	if got := balance(t, s, tenant, alice); got != 7_500 {
		t.Fatalf("alice = %d, want 7500", got)
	}
	if got := balance(t, s, tenant, world); got != -10_000 {
		t.Fatalf("world = %d, want -10000", got)
	}

	got, err := s.GetTransaction(context.Background(), tenant, txn.GetId())
	if err != nil || len(got.GetPostings()) != 2 || got.GetPostings()[1].GetBalanceAfter() != 2_500 {
		t.Fatalf("GetTransaction = %v, %v", got, err)
	}

	entries, _, err := s.ListEntries(context.Background(), tenant, alice, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].GetBalanceAfter() != 7_500 || entries[1].GetBalanceAfter() != 10_000 {
		t.Fatalf("statement wrong: %v", entries)
	}
}

func TestOverdraftRejected(t *testing.T) {
	t.Parallel()
	s, tenant := testStore(t), newTenant()
	alice := mustAccount(t, s, tenant, "USD", false)
	bob := mustAccount(t, s, tenant, "USD", false)
	if _, err := transfer(s, tenant, alice, bob, 1, key()); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("got %v, want ErrInsufficientFunds", err)
	}
}

// 50 concurrent withdrawals of 10 from an account holding 100: exactly 10
// succeed, and the balance ends at 0, never below.
func TestConcurrentWithdrawalsCannotOverdraw(t *testing.T) {
	t.Parallel()
	s, tenant := testStore(t), newTenant()
	world := mustAccount(t, s, tenant, "USD", true)
	alice := mustAccount(t, s, tenant, "USD", false)
	if _, err := transfer(s, tenant, world, alice, 100, key()); err != nil {
		t.Fatal(err)
	}

	var ok, rejected atomic.Int64
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			_, err := transfer(s, tenant, alice, world, 10, key())
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ledger.ErrInsufficientFunds):
				rejected.Add(1)
			default:
				t.Error(err)
			}
		})
	}
	wg.Wait()

	if ok.Load() != 10 || rejected.Load() != 40 {
		t.Fatalf("succeeded=%d rejected=%d, want 10/40", ok.Load(), rejected.Load())
	}
	if got := balance(t, s, tenant, alice); got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
}

func TestIdempotentRetry(t *testing.T) {
	t.Parallel()
	s, tenant := testStore(t), newTenant()
	world := mustAccount(t, s, tenant, "USD", true)
	alice := mustAccount(t, s, tenant, "USD", false)

	k := key()
	first, err := transfer(s, tenant, world, alice, 500, k)
	if err != nil {
		t.Fatal(err)
	}
	again, err := transfer(s, tenant, world, alice, 500, k)
	if err != nil {
		t.Fatal(err)
	}
	if again.GetId() != first.GetId() {
		t.Fatalf("retry created a new transaction: %s vs %s", again.GetId(), first.GetId())
	}
	if got := balance(t, s, tenant, alice); got != 500 {
		t.Fatalf("balance = %d, want 500 (posted once)", got)
	}

	if _, err := transfer(s, tenant, world, alice, 501, k); !errors.Is(err, ledger.ErrIdempotencyMismatch) {
		t.Fatalf("reused key with a different amount: got %v", err)
	}
}

// 20 concurrent requests with the same key: one transaction, 20 identical answers.
func TestConcurrentIdempotentRequests(t *testing.T) {
	t.Parallel()
	s, tenant := testStore(t), newTenant()
	world := mustAccount(t, s, tenant, "USD", true)
	alice := mustAccount(t, s, tenant, "USD", false)

	k := key()
	ids := make(chan string, 20)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			txn, err := transfer(s, tenant, world, alice, 700, k)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- txn.GetId()
		})
	}
	wg.Wait()
	close(ids)

	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	if len(unique) != 1 {
		t.Fatalf("got %d distinct transactions, want 1", len(unique))
	}
	if got := balance(t, s, tenant, alice); got != 700 {
		t.Fatalf("balance = %d, want 700", got)
	}
}

func TestFailedRequestCanBeRetried(t *testing.T) {
	t.Parallel()
	s, tenant := testStore(t), newTenant()
	world := mustAccount(t, s, tenant, "USD", true)
	alice := mustAccount(t, s, tenant, "USD", false)
	bob := mustAccount(t, s, tenant, "USD", false)

	k := key()
	if _, err := transfer(s, tenant, alice, bob, 100, k); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("got %v", err)
	}
	if _, err := transfer(s, tenant, world, alice, 100, key()); err != nil {
		t.Fatal(err)
	}
	// Same key, same request: the failure wasn't remembered, so it now succeeds.
	if _, err := transfer(s, tenant, alice, bob, 100, k); err != nil {
		t.Fatalf("retry after funding failed: %v", err)
	}
}

func TestTenantIsolation(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	acme, globex := newTenant(), newTenant()
	acmeWorld := mustAccount(t, s, acme, "USD", true)
	acmeAlice := mustAccount(t, s, acme, "USD", false)
	globexAcct := mustAccount(t, s, globex, "USD", false)

	if _, err := s.GetAccount(context.Background(), globex, acmeAlice); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("globex read acme's account: %v", err)
	}
	// Even knowing acme's account IDs, globex can't move acme's money.
	if _, err := transfer(s, globex, acmeWorld, globexAcct, 100, key()); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("globex posted against acme's account: %v", err)
	}
	accounts, _, err := s.ListAccounts(context.Background(), globex, 200, "")
	if err != nil || len(accounts) != 1 {
		t.Fatalf("globex sees %d accounts (%v), want 1", len(accounts), err)
	}
	// Idempotency keys are per tenant too.
	shared := key()
	if _, err := transfer(s, acme, acmeWorld, acmeAlice, 1, shared); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAccount(context.Background(), globex, &ledgerv1.CreateAccountRequest{
		IdempotencyKey: shared, Name: "x", Currency: "USD"}); err != nil {
		t.Fatalf("globex blocked by acme's idempotency key: %v", err)
	}
}

func TestCrossCurrencyRejected(t *testing.T) {
	t.Parallel()
	s, tenant := testStore(t), newTenant()
	usd := mustAccount(t, s, tenant, "USD", true)
	eur := mustAccount(t, s, tenant, "EUR", true)
	if _, err := transfer(s, tenant, usd, eur, 100, key()); !errors.Is(err, ledger.ErrInvalid) {
		t.Fatalf("got %v, want ErrInvalid", err)
	}
}

func TestReverseOnlyOnce(t *testing.T) {
	t.Parallel()
	s, tenant := testStore(t), newTenant()
	world := mustAccount(t, s, tenant, "USD", true)
	alice := mustAccount(t, s, tenant, "USD", false)
	txn, err := transfer(s, tenant, world, alice, 300, key())
	if err != nil {
		t.Fatal(err)
	}

	var ok, already atomic.Int64
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			_, err := s.ReverseTransaction(context.Background(), tenant, &ledgerv1.ReverseTransactionRequest{
				IdempotencyKey: key(), TransactionId: txn.GetId()})
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ledger.ErrAlreadyReversed):
				already.Add(1)
			default:
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 1 || already.Load() != 9 {
		t.Fatalf("reversed %d times (%d rejected), want exactly once", ok.Load(), already.Load())
	}
	if got := balance(t, s, tenant, alice); got != 0 {
		t.Fatalf("balance after reversal = %d", got)
	}
}

func TestReversalOfReversalRejected(t *testing.T) {
	t.Parallel()
	s, tenant := testStore(t), newTenant()
	world := mustAccount(t, s, tenant, "USD", true)
	alice := mustAccount(t, s, tenant, "USD", false)
	txn, _ := transfer(s, tenant, world, alice, 300, key())
	rev, err := s.ReverseTransaction(context.Background(), tenant, &ledgerv1.ReverseTransactionRequest{
		IdempotencyKey: key(), TransactionId: txn.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ReverseTransaction(context.Background(), tenant, &ledgerv1.ReverseTransactionRequest{
		IdempotencyKey: key(), TransactionId: rev.GetId()})
	if !errors.Is(err, ledger.ErrNotReversible) {
		t.Fatalf("got %v, want ErrNotReversible", err)
	}
}

// The database rejects an unbalanced transaction even if application code
// were bypassed: here we insert postings directly as ledger_app.
func TestDatabaseRejectsUnbalancedTransaction(t *testing.T) {
	t.Parallel()
	s, tenant := testStore(t), newTenant()
	a := mustAccount(t, s, tenant, "USD", true)
	b := mustAccount(t, s, tenant, "USD", true)
	err := s.inTenant(context.Background(), tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(),
			`INSERT INTO transactions (tenant_id, id) VALUES ($1, 'txn_raw')`, tenant); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), `
			INSERT INTO postings (tenant_id, transaction_id, seq, account_id, currency, amount, balance_after, account_version)
			VALUES ($1, 'txn_raw', 0, $2, 'USD', -100, -100, 1), ($1, 'txn_raw', 1, $3, 'USD', 99, 99, 1)`,
			tenant, a, b)
		return err
	})
	if !errors.Is(err, ledger.ErrInvalid) {
		t.Fatalf("unbalanced transaction committed or wrong error: %v", err)
	}
}

func TestDatabaseRejectsHistoryEdits(t *testing.T) {
	t.Parallel()
	s, tenant := testStore(t), newTenant()
	world := mustAccount(t, s, tenant, "USD", true)
	alice := mustAccount(t, s, tenant, "USD", false)
	if _, err := transfer(s, tenant, world, alice, 100, key()); err != nil {
		t.Fatal(err)
	}
	err := s.inTenant(context.Background(), tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE postings SET amount = amount * 2`)
		return err
	})
	if err == nil {
		t.Fatal("postings were editable")
	}
}
