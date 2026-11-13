# 2. Pessimistic row locks in a fixed order

**Status:** accepted

## Context

Two transfers that touch the same account must not both read the old balance
and write their own new one: that's a lost update, and it can also let an
account overdraw. Options:

| Approach | Problem |
|---|---|
| `SERIALIZABLE` isolation | correct, but conflicts surface as serialization failures that the caller must retry. Under contention the retry rate climbs. |
| Optimistic locking (`UPDATE … WHERE version = $seen`) | same retry storm on busy accounts, and more code paths |
| `SELECT … FOR UPDATE` | waits instead of failing; can deadlock if two transfers lock A→B and B→A |

## Decision

`PostTransaction` locks every account involved with `SELECT … FOR UPDATE`,
**always in ascending account-ID order**. Two transfers over the same accounts
then always request locks in the same order, so neither can hold one lock
while waiting for the other's. That rules out deadlocks between them.

Inside the lock, the new balance, the account version and each posting's
`balance_after` are computed and written. `balance_after` turns every account
into a self-verifying statement: the newest entry's `balance_after` must
equal the balance, and versions must run 1…n without gaps. The
property-based test checks both after every step.

As a safety net, the store retries a transaction up to 5 times on `40P01`
(deadlock) or `40001` (serialization failure).

## Consequences

- `TestConcurrentWithdrawalsCannotOverdraw`: 50 concurrent withdrawals of 10
  from a balance of 100. Exactly 10 succeed and the balance ends at 0.
- **Hot accounts serialise.** Every transfer funded by one account waits for
  that row's lock. The benchmark shows it: 2,365 txn/s for spread traffic,
  720 txn/s when one account funds everything. Known fixes, in the order I'd
  reach for them:
  1. **Sub-accounts / sharded balances.** Split the hot account into N
     buckets and pick one per transfer. The real balance is the sum.
  2. **Batching.** Aggregate many small postings to the hot account into one
     posting per batch window.
  3. **Allow-negative accounts don't need the lock for correctness.** An
     external or funding account that may go negative could be updated with
     a single atomic `UPDATE … SET balance = balance + $1`, with no
     read-modify-write. The catch is that you'd lose its `balance_after`
     sequence.
- Reversals can't lock the original transaction row (rows are append-only, and
  `FOR UPDATE` needs `UPDATE` privilege), so they take a transaction-scoped
  advisory lock on the transaction ID instead. A concurrent second reversal
  then reliably reports "already reversed" rather than an unrelated error.
  The integration test caught exactly that bug.
