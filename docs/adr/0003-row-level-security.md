# 3. Tenant isolation with Postgres row-level security

**Status:** accepted

## Context

All tenants share one database. The usual approach, adding
`WHERE tenant_id = $1` to every query, works until one query forgets it.
In a ledger, that one query is a data breach.

## Decision

Two layers:

1. **The tenant comes from the access token** (`tenant_id` claim), set by the
   identity provider. It isn't a request field, so a caller can't ask for
   another tenant.
2. **Postgres row-level security** enforces it in the database. Every
   transaction starts with `SELECT set_config('app.tenant_id', $tenant, true)`
   (transaction-local, so it can't leak to the next user of a pooled
   connection). Every table has a policy:

   ```sql
   USING      (tenant_id = current_setting('app.tenant_id', true))
   WITH CHECK (tenant_id = current_setting('app.tenant_id', true))
   ```

   The service connects as `ledger_app`, which doesn't own the tables and
   has `NOBYPASSRLS`, so the policies always apply.

## Consequences

- **It fails closed.** If code ever forgets `set_config`, `current_setting`
  returns `NULL`, the comparison is never true, and queries see no rows and
  can insert none.
- Queries in the store carry no tenant filter in `WHERE` clauses. That's
  deliberate: isolation is the database's job, and the tests prove it
  (`TestTenantIsolation`: another tenant gets `NotFound` even with exact
  account IDs, and can't post against them).
- Primary keys include `tenant_id`, so idempotency keys and IDs are scoped per
  tenant too, and one tenant's keys can't block another's.
- Table owners and superusers bypass RLS. Migrations and admin tooling run as
  the owner, so that role must be guarded like any other production
  credential.
