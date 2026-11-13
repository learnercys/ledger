# 1. Enforce money rules in the database, not only in code

**Status:** accepted

## Context

A ledger has a few rules that must never break: transactions balance,
accounts don't overdraw, history isn't edited, and tenants don't see each
other. Application code checks all of them, but application code has bugs,
gets bypassed by scripts and migrations, and changes over time.

## Decision

Each rule has a database-level backstop, so bad money can't be committed
whatever the application does:

| Rule | Application check | Database backstop |
|---|---|---|
| Postings sum to zero per currency | `ledger.CheckBalanced` | `DEFERRABLE INITIALLY DEFERRED` constraint trigger, checked at `COMMIT` |
| No overdraft unless `allow_negative` | computed under row lock in `post()` | `CHECK (allow_negative OR balance >= 0)` |
| History is append-only | no update/delete code paths | `ledger_app` has no `UPDATE`/`DELETE` grant on postings and transactions, plus a trigger that rejects edits even by the owner |
| A transaction is reversed at most once | advisory lock + existence check | `UNIQUE (tenant_id, reverses_id)` |
| Tenant isolation | tenant comes from the token | row-level security ([ADR 3](0003-row-level-security.md)) |
| Statements have no gaps | version computed under row lock | `UNIQUE (tenant_id, account_id, account_version)` |

The balance trigger is **deferred** because a transaction's postings are
inserted together: checking after each row would reject every multi-leg
transaction halfway through.

## Consequences

- Integration tests prove each backstop independently. For example,
  `TestDatabaseRejectsUnbalancedTransaction` inserts unbalanced postings with
  raw SQL as the service's own role, and the commit fails.
- Constraint errors are translated back into domain errors
  (`balance_non_negative` becomes `ErrInsufficientFunds`), so callers get the
  same answer whichever layer caught the problem.
- The deferred trigger costs one aggregate query per posting at commit. With
  2–4 postings per transaction this doesn't show up in the benchmark. At
  hundreds of legs, a statement-level trigger with transition tables would be
  cheaper.
