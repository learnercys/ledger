//go:build integration

package store

import (
	"context"
	"errors"
	"testing"

	"pgregory.net/rapid"

	ledgerv1 "github.com/learnercys/ledger/gen/ledger/v1"
	"github.com/learnercys/ledger/internal/ledger"
)

// TestLedgerProperties drives the real store with random sequences of
// operations, including invalid ones, and compares it with a trivial
// in-memory model after every step. rapid shrinks any failure to a minimal
// sequence of operations.
//
// Invariants checked after every step:
//   - each balance equals the model (no lost or duplicated postings)
//   - balances sum to zero per currency (money is never created or destroyed)
//   - no account without allow_negative is below zero
//   - each account's statement is gap-free: versions 1..n, and the newest
//     entry's balance_after equals the account balance
func TestLedgerProperties(t *testing.T) {
	s := testStore(t)
	rapid.Check(t, func(rt *rapid.T) {
		m := &model{t: rt, s: s, tenant: newTenant(), balance: map[string]int64{},
			currency: map[string]string{}, allowNegative: map[string]bool{}, reversed: map[string]bool{}}
		m.open("USD", true)
		m.open("USD", false)
		m.open("EUR", true)
		rt.Repeat(map[string]func(*rapid.T){
			"open": func(t *rapid.T) {
				m.open(rapid.SampledFrom([]string{"USD", "EUR"}).Draw(t, "cur"), rapid.Bool().Draw(t, "neg"))
			},
			"transfer": m.transfer,
			"multileg": m.multileg,
			"retry":    m.retry,
			"reverse":  m.reverse,
			"":         m.check,
		})
	})
}

type posted struct {
	req *ledgerv1.PostTransactionRequest
	txn *ledgerv1.Transaction
}

type model struct {
	t             *rapid.T
	s             *Store
	tenant        string
	accounts      []string
	balance       map[string]int64
	currency      map[string]string
	allowNegative map[string]bool
	history       []posted
	reversed      map[string]bool
}

func (m *model) open(cur string, allowNegative bool) {
	a, err := m.s.CreateAccount(context.Background(), m.tenant, &ledgerv1.CreateAccountRequest{
		IdempotencyKey: key(), Name: "p", Currency: cur, AllowNegative: allowNegative})
	if err != nil {
		m.t.Fatal(err)
	}
	m.accounts = append(m.accounts, a.GetId())
	m.currency[a.GetId()] = cur
	m.allowNegative[a.GetId()] = allowNegative
}

// expect predicts whether postings should succeed, mirroring the ledger rules.
func (m *model) expect(ps []*ledgerv1.Posting) error {
	sums := map[string]int64{}
	next := map[string]int64{}
	for _, p := range ps {
		sums[m.currency[p.GetAccountId()]] += p.GetAmount()
		next[p.GetAccountId()] = m.balance[p.GetAccountId()] + p.GetAmount()
	}
	for _, s := range sums {
		if s != 0 {
			return ledger.ErrInvalid
		}
	}
	for id, b := range next {
		if b < 0 && !m.allowNegative[id] {
			return ledger.ErrInsufficientFunds
		}
	}
	return nil
}

func (m *model) apply(ps []*ledgerv1.Posting) {
	for _, p := range ps {
		m.balance[p.GetAccountId()] += p.GetAmount()
	}
}

func (m *model) submit(ps []*ledgerv1.Posting) {
	req := &ledgerv1.PostTransactionRequest{IdempotencyKey: key(), Postings: ps}
	want := m.expect(ps)
	txn, err := m.s.PostTransaction(context.Background(), m.tenant, req)
	if !errors.Is(err, want) && !(err == nil && want == nil) {
		m.t.Fatalf("postings %v: got error %v, model expected %v", ps, err, want)
	}
	if err == nil {
		m.apply(ps)
		m.history = append(m.history, posted{req, txn})
	}
}

func (m *model) transfer(t *rapid.T) {
	from := rapid.SampledFrom(m.accounts).Draw(t, "from")
	to := rapid.SampledFrom(m.accounts).Filter(func(id string) bool { return id != from }).Draw(t, "to")
	amount := rapid.Int64Range(1, 5_000).Draw(t, "amount")
	m.submit([]*ledgerv1.Posting{{AccountId: from, Amount: -amount}, {AccountId: to, Amount: amount}})
}

// multileg splits one debit across several credits in the same currency.
func (m *model) multileg(t *rapid.T) {
	cur := rapid.SampledFrom([]string{"USD", "EUR"}).Draw(t, "cur")
	var same []string
	for _, id := range m.accounts {
		if m.currency[id] == cur {
			same = append(same, id)
		}
	}
	if len(same) < 3 {
		t.Skip("not enough accounts")
	}
	ids := rapid.Permutation(same).Draw(t, "order")[:3]
	a := rapid.Int64Range(1, 3_000).Draw(t, "a")
	b := rapid.Int64Range(1, 3_000).Draw(t, "b")
	m.submit([]*ledgerv1.Posting{
		{AccountId: ids[0], Amount: -(a + b)}, {AccountId: ids[1], Amount: a}, {AccountId: ids[2], Amount: b},
	})
}

// retry re-sends an earlier request with its original key: it must return
// the original transaction and change nothing.
func (m *model) retry(t *rapid.T) {
	if len(m.history) == 0 {
		t.Skip("nothing to retry")
	}
	p := rapid.SampledFrom(m.history).Draw(t, "earlier")
	if p.req == nil {
		t.Skip("reversals are retried via ReverseTransaction")
	}
	txn, err := m.s.PostTransaction(context.Background(), m.tenant, p.req)
	if err != nil || txn.GetId() != p.txn.GetId() {
		t.Fatalf("retry of %s returned %v, %v", p.txn.GetId(), txn.GetId(), err)
	}
}

func (m *model) reverse(t *rapid.T) {
	if len(m.history) == 0 {
		t.Skip("nothing to reverse")
	}
	p := rapid.SampledFrom(m.history).Draw(t, "target")
	if p.txn.GetReversesId() != "" {
		t.Skip("reversal of a reversal")
	}
	undo := make([]*ledgerv1.Posting, len(p.txn.GetPostings()))
	for i, q := range p.txn.GetPostings() {
		undo[i] = &ledgerv1.Posting{AccountId: q.GetAccountId(), Amount: -q.GetAmount()}
	}
	want := m.expect(undo)
	if m.reversed[p.txn.GetId()] {
		want = ledger.ErrAlreadyReversed
	}
	rev, err := m.s.ReverseTransaction(context.Background(), m.tenant, &ledgerv1.ReverseTransactionRequest{
		IdempotencyKey: key(), TransactionId: p.txn.GetId()})
	if !errors.Is(err, want) && !(err == nil && want == nil) {
		t.Fatalf("reverse %s: got %v, model expected %v", p.txn.GetId(), err, want)
	}
	if err == nil {
		m.apply(undo)
		m.reversed[p.txn.GetId()] = true
		m.history = append(m.history, posted{nil, rev})
	}
}

func (m *model) check(t *rapid.T) {
	ctx := context.Background()
	sums := map[string]int64{}
	for _, id := range m.accounts {
		a, err := m.s.GetAccount(ctx, m.tenant, id)
		if err != nil {
			t.Fatal(err)
		}
		if a.GetBalance() != m.balance[id] {
			t.Fatalf("account %s: balance %d, model says %d", id, a.GetBalance(), m.balance[id])
		}
		if a.GetBalance() < 0 && !a.GetAllowNegative() {
			t.Fatalf("account %s overdrawn: %d", id, a.GetBalance())
		}
		sums[a.GetCurrency()] += a.GetBalance()

		entries, _, err := m.s.ListEntries(ctx, m.tenant, id, 200, "")
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(entries)) != a.GetVersion() {
			t.Fatalf("account %s: %d entries but version %d", id, len(entries), a.GetVersion())
		}
		for i, e := range entries {
			if e.GetAccountVersion() != a.GetVersion()-int64(i) {
				t.Fatalf("account %s: statement has a gap at version %d", id, e.GetAccountVersion())
			}
		}
		if len(entries) > 0 && entries[0].GetBalanceAfter() != a.GetBalance() {
			t.Fatalf("account %s: latest balance_after %d != balance %d", id, entries[0].GetBalanceAfter(), a.GetBalance())
		}
	}
	for cur, sum := range sums {
		if sum != 0 {
			t.Fatalf("money created or destroyed: %s sums to %d", cur, sum)
		}
	}
}
