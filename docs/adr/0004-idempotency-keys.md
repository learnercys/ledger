# 4. Idempotency keys stored in the same transaction as the money

**Status:** accepted

## Context

Clients retry: timeouts, dropped connections, crashed workers. A retried
transfer must not move money twice, and the client should get the same answer
it would have got the first time.

## Decision

Every write (`CreateAccount`, `PostTransaction`, `ReverseTransaction`)
requires an `idempotency_key`. In the **same database transaction** as the
write:

```
INSERT INTO idempotency_keys (tenant_id, key, method, request_hash) … ON CONFLICT DO NOTHING
  inserted?  → do the work, store the serialized response, COMMIT
  conflict?  → same request_hash → return the stored response, do nothing
               different hash    → AlreadyExists ("key reused for a different request")
```

- **Concurrent retries** are handled by Postgres. The second `INSERT` blocks
  on the first transaction's uncommitted row, then sees the conflict once
  it commits. `TestConcurrentIdempotentRequests` fires 20 identical requests
  at once and gets exactly one transaction.
- **The hash** covers the method and the request with its key cleared
  (deterministic protobuf encoding). The same key with a different amount is
  rejected, not silently answered with the old result.
- **Only successes are remembered.** If the work fails, for example with
  insufficient funds, the key row rolls back with everything else, and the
  client can retry the same request once the cause is fixed
  (`TestFailedRequestCanBeRetried`).

## Consequences

- Keys are scoped per tenant (part of the primary key), so tenants can't
  collide.
- The table grows with traffic. Production would expire keys after a
  retention window (24h–7d is common) with a scheduled delete or daily
  partitions.
- Some APIs (Stripe, for one) also cache error responses. I chose not to: for
  a ledger, "retry after topping up" is a legitimate flow, and an error has no
  side effects worth protecting.
