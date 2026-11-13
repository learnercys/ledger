#!/usr/bin/env bash
# End-to-end demo against the running stack (`make up`): real Keycloak tokens,
# plain curl, JSON over HTTP. The same API also speaks gRPC on the same port.
set -euo pipefail

KEYCLOAK=http://localhost:58180/realms/ledger/protocol/openid-connect/token
API=http://localhost:58090/ledger.v1.LedgerService

token() { # client_id secret
  curl -sf -d grant_type=client_credentials -d client_id="$1" -d client_secret="$2" "$KEYCLOAK" |
    python3 -c 'import sys, json; print(json.load(sys.stdin)["access_token"])'
}

# rpc TOKEN METHOD JSON  -> prints the response body and HTTP status
rpc() {
  curl -s -w '\n  [HTTP %{http_code}]\n' -X POST "$API/$2" \
    -H "Authorization: Bearer $1" -H 'Content-Type: application/json' -d "$3"
}

field() { python3 -c "import sys, json; print(json.load(sys.stdin)$1)"; }

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

RUN=$(date +%s)
ACME=$(token acme-service acme-secret)
ACME_RO=$(token acme-readonly acme-readonly-secret)
GLOBEX=$(token globex-service globex-secret)

step "acme opens a funding account (may go negative) and two customer accounts"
FUNDING=$(rpc "$ACME" CreateAccount "{\"idempotencyKey\":\"$RUN-f\",\"name\":\"Card settlements\",\"currency\":\"USD\",\"allowNegative\":true}" | head -1 | field '["account"]["id"]')
ALICE=$(rpc "$ACME" CreateAccount "{\"idempotencyKey\":\"$RUN-a\",\"name\":\"Alice\",\"currency\":\"USD\"}" | head -1 | field '["account"]["id"]')
BOB=$(rpc "$ACME" CreateAccount "{\"idempotencyKey\":\"$RUN-b\",\"name\":\"Bob\",\"currency\":\"USD\"}" | head -1 | field '["account"]["id"]')
echo "funding=$FUNDING alice=$ALICE bob=$BOB"

step "fund Alice with \$100.00"
rpc "$ACME" PostTransaction "{\"idempotencyKey\":\"$RUN-t1\",\"description\":\"Top-up\",\"postings\":[{\"accountId\":\"$FUNDING\",\"amount\":\"-10000\"},{\"accountId\":\"$ALICE\",\"amount\":\"10000\"}]}"

step "Alice pays Bob \$25.00, and the client retries with the same idempotency key"
PAY="{\"idempotencyKey\":\"$RUN-t2\",\"description\":\"Dinner\",\"postings\":[{\"accountId\":\"$ALICE\",\"amount\":\"-2500\"},{\"accountId\":\"$BOB\",\"amount\":\"2500\"}]}"
rpc "$ACME" PostTransaction "$PAY"
rpc "$ACME" PostTransaction "$PAY"
echo "(same transaction ID both times; Bob was paid once)"

step "Alice tries to pay \$500.00 she doesn't have"
rpc "$ACME" PostTransaction "{\"idempotencyKey\":\"$RUN-t3\",\"postings\":[{\"accountId\":\"$ALICE\",\"amount\":\"-50000\"},{\"accountId\":\"$BOB\",\"amount\":\"50000\"}]}"

step "an unbalanced transaction is rejected"
rpc "$ACME" PostTransaction "{\"idempotencyKey\":\"$RUN-t4\",\"postings\":[{\"accountId\":\"$ALICE\",\"amount\":\"-100\"},{\"accountId\":\"$BOB\",\"amount\":\"99\"}]}"

step "the read-only client can read Alice's statement..."
rpc "$ACME_RO" ListEntries "{\"accountId\":\"$ALICE\"}"

step "...but can't move money"
rpc "$ACME_RO" PostTransaction "{\"idempotencyKey\":\"$RUN-t5\",\"postings\":[]}"

step "globex can't see acme's accounts, even with the exact ID"
rpc "$GLOBEX" GetAccount "{\"id\":\"$ALICE\"}"

step "no token at all"
curl -s -w '\n  [HTTP %{http_code}]\n' -X POST "$API/GetAccount" -H 'Content-Type: application/json' -d "{\"id\":\"$ALICE\"}"
