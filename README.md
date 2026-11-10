# SumUp-API

An HTTP API for creating accounts, depositing money, and transferring money
between accounts, with correct balances under concurrent load. It is written
in Go 1.27, uses PostgreSQL, and has one direct dependency, the
`pgx` driver.

## Functional Requirements

 - Account creation requires name, address, date_of_birth (YYYY-MM-DD), and id_number. `initial_balance` is optional.
 - Allow deposits to be made. 
 - Transfers between accounts so the API is useable for customers/users.
 - Simple ledger system for auditing and system state.

## Non-functional Requirements

 - Focus on Consistency and correctness for this section of the Bank API.
 - Availability is nice, but probably less important than missing money or overdrafts, redundant txns.
 - If high throughput is needed, there are ways to balance. (Sub-accounts, pinning resources on "hot" accounts, etc). 

## Quick start

You need Docker with Compose. Install from here: https://docs.docker.com/desktop/setup/install/mac-install/

```sh
docker compose up --build
```

This starts PostgreSQL, applies `schema.sql`, and serves the API on
`http://localhost:8080`. Accounts are kept on a volume, so they survive
restarts and rebuilds. `docker compose down -v` deletes them.

Try it from another terminal (the example uses `curl` and `jq`):

```sh
A=$(curl -s localhost:8080/accounts -d '{
  "name": "Ada Lovelace",
  "address": "12 St James Square, London",
  "date_of_birth": "1985-12-10",
  "id_number": "AB123456",
  "initial_balance": 1000
}' | jq -r .id)
# 65M455BCQOQL4QLELJUDD6ETXZ
B=$(curl -s localhost:8080/accounts -d '{
  "name": "Alan Turing",
  "address": "78 High Street, Hampton",
  "date_of_birth": "1990-06-23",
  "id_number": "CD789012"
}' | jq -r .id)
# VGEXGDHH6UQENGP6DVCGAC5FTD

curl -s localhost:8080/accounts/$B/deposits -d '{"amount": 250}' | jq
# {
#   "id": "VGEXGDHH6UQENGP6DVCGAC5FTD",
#   "balance": 250
# }

curl -s localhost:8080/transfers -d "{\"from\": \"$A\", \"to\": \"$B\", \"amount\": 400}" | jq
# {
#   "from": {
#     "id": "65M455BCQOQL4QLELJUDD6ETXZ",
#     "balance": 600
#   },
#   "to": {
#     "id": "VGEXGDHH6UQENGP6DVCGAC5FTD",
#     "balance": 650
#   }
# }
curl -s localhost:8080/transfers -d "{\"from\": \"$A\", \"to\": \"$B\", \"amount\": 999999}" | jq
# HTTP/1.1 409 Conflict
# {
#   "error": "insufficient funds"
# }

curl -s localhost:8080/accounts/$B/ledger | jq
# {
#   "entries": [
#     {
#       "id": 4,
#       "transaction_id": "T3JZ7G2Q5NHD4XK6M2VPLCW8RA",
#       "type": "transfer",
#       "amount": 400,
#       "balance_after": 650,
#       "counterparty_account_id": "65M455BCQOQL4QLELJUDD6ETXZ",
#       "created_at": "2026-11-10T12:00:01.482913Z"
#     },
#     {
#       "id": 2,
#       "transaction_id": "H8PLD5VQ2ZKM7WXC3N6TJRB4EA",
#       "type": "deposit",
#       "amount": 250,
#       "balance_after": 250,
#       "created_at": "2026-11-10T12:00:00.917204Z"
#     }
#   ]
# }
```

### Postman or Bruno

Import `SumUp-API.postman_collection.json` into either tool. With the stack
running, run the collection in order: `Happy Path` creates the accounts that
`Error Cases` uses. Every request asserts on its response, and each run generates
new ID numbers, so it can be run repeatedly.

## API

Amounts are integers in the currency's minor unit, so `1050` means 10.50. An
account is returned as `{"id": "...", "balance": 1000}`.

| Route                          | Body                                      | Success                             |
| ------------------------------ | ----------------------------------------- | ----------------------------------- |
| `POST /accounts`               | Holder details (below)                    | `201`, the account, and `Location`  |
| `GET /accounts/{id}`           | None                                      | `200` and the account               |
| `GET /accounts/{id}/ledger`    | None; optional `?before={entry id}`       | `200` and up to 100 entries, newest first |
| `POST /accounts/{id}/deposits` | `{"amount": 500}`                         | `200` and the updated account       |
| `POST /transfers`              | `{"from": "ID", "to": "ID", "amount": 1}` | `200` and `{"from": ..., "to": ...}` with both updated accounts |

`POST /accounts` requires `name`, `address`, `date_of_birth` (`YYYY-MM-DD`), and
`id_number`. `initial_balance` is optional and defaults to zero. Each ID number
can open one account; letter case, spaces, and hyphens are ignored when comparing
them. Holder details are stored but never returned, because the API has no
authentication.

A signup is also rejected when an existing account has the same `date_of_birth`
and both a similar `name` and a similar `id_number`, meaning within 3 edits by
Levenshtein distance. Names are compared without regard to case.

When a ledger response includes `next_before`, pass it as `?before=` to fetch the
next page.

Every error, including an unknown route, is JSON: `{"error": "message"}`.

| Status | Meaning                                                                                       |
| ------ | --------------------------------------------------------------------------------------------- |
| `400`  | Malformed body, unknown or invalid field, missing or invalid holder details, missing `from` or `to`, non-positive amount, negative initial balance, invalid `before`, or a transfer to the same account |
| `404`  | The account or route does not exist                                                           |
| `405`  | The route does not support the method; `Allow` lists those it does                            |
| `409`  | Insufficient funds, the destination balance would overflow, or the ID number or a similar holder already has an account |
| `413`  | The body is larger than 64 KiB                                                                |
| `500`  | Unexpected failure; details are logged and not returned                                       |
| `503`  | The request hit its 5-second limit, and the txn was rolled back |


## Design

```text
main.go         wiring, configuration, graceful shutdown
internal/api    HTTP: routing, JSON, and mapping errors to status codes
internal/bank   account and holder rules, the ledger, PostgreSQL transactions
schema.sql      the accounts and ledger_entries tables
```

`internal/bank` knows nothing about HTTP. `internal/api` depends only on a small
`Bank` interface that it defines itself, and `main.go` joins the two.

### Consistency under concurrency

A deposit or transfer runs as one transaction:

1. `SELECT ... ORDER BY id FOR UPDATE` locks the rows involved. A competing
   request that touches either account waits here and then sees the committed
   balance.
2. Go code checks the funds and the overflow limit against the locked values.
3. A ledger entry is inserted for each account, which also updates its balance,
   and the transaction commits. Any failure rolls back the whole operation.

This gives the following guarantees:

- **No overdrafts.** The funds check and the writes happen under the same lock.
  A `CHECK (balance >= 0)` constraint is a second line of defense.
- **No lost updates or partial transfers.** Money is never visible as having
  left one account without reaching the other.
- **No deadlocks.** Rows are always locked in ID order, so opposing transfers
  (A to B and B to A) cannot wait on each other.
- **Parallel writes.** Locks are per row, so transfers between unrelated
  accounts do not wait for each other.
- **Bounded waits.** Each request has a 5-second deadline, so a stuck lock
  cannot hold a request or a connection indefinitely.
- **No duplicate signups.** A unique constraint guards exact ID numbers. It
  cannot express similarity, so signups with the same date of birth take turns
  under an advisory lock while the similarity check runs. Signups with different
  dates of birth stay parallel.

### Ledger

`ledger_entries` records every change to a balance, with one row per account
touched. A positive `amount` is money in and a negative one is money out.

- **Balances and entries cannot diverge.** The database enforces this, not the
  application. Inserting a ledger entry is the only way to change a balance: a
  trigger applies the amount and sets `balance_after` within that one `INSERT`,
  so both succeed or fail together. Another trigger rejects any other write to
  `accounts.balance`, including from direct SQL. Only a database administrator
  who deliberately disables the triggers could get around this.
- **Transfers net to zero.** A transfer writes two entries that share a
  `transaction_id`. Deposits and opening balances have one entry, because that
  money comes from outside the system.
- **History is immutable.** Database triggers reject any `UPDATE`, `DELETE`, or
  `TRUNCATE` on the table, so corrections must be new entries.
- **Entries are ordered per account.** IDs are assigned while the row is locked,
  so each account's `balance_after` values form an unbroken chain.

To confirm every balance matches its ledger (this prints `0` when they do):

```sh
docker compose exec db psql -U bank -d bank -tAc "
  SELECT count(*) FROM accounts a
  WHERE a.balance <> (SELECT COALESCE(SUM(amount), 0) FROM ledger_entries e WHERE e.account_id = a.id)"
```

### Other decisions

- **Money** is `int64` minor units, which avoids floating-point rounding.
  Credits are checked for overflow.
- **JSON** uses `encoding/json/v2`. Duplicate keys, wrongly cased keys, unknown
  fields, and trailing data are all rejected, so a body like
  `{"amount": 1, "amount": 1000}` cannot be read two ways. Error messages name
  the offending field and never expose internal types.

## Testing

```sh
docker compose -f compose.test.yaml up --attach test --exit-code-from test
```

This lists each test by name with its result, followed by a summary. Every run
uses the race detector and skips Go's test cache, against a throwaway PostgreSQL
that is separate from your data. It also writes a line-by-line coverage report
to `coverage/coverage.html`, which you can open in a browser. Plain `go test` fails with a clear message because
the tests need a real database. Locking behavior is the riskiest part of this
service, and a mock could not exercise it.

- `internal/bank` tests the rules with table-driven cases and the concurrency
  guarantees directly: racing transfers cannot overdraw, random transfers
  conserve the total, opposing transfers do not deadlock, only transfers
  sharing an account wait for each other, and concurrent identical or similar
  signups create one account. After concurrent runs, the tests also reconcile the
  ledger against the balances. Direct SQL tests confirm that nothing outside the
  ledger can move a balance and that a failed second leg undoes the first.
- `internal/api` tests the HTTP contract through the real stack: a full money
  flow, plus the exact status and message for each error. A small stub supplies
  infrastructure failures that are hard to provoke, such as timeouts.

## Limitations

These are deliberate omissions for an exercise of this size:

- **Retries are not idempotent.** A client that retries after a timeout or a
  dropped connection can apply a transfer twice, and a `503` during commit does
  not prove the transfer failed. Production use needs an `Idempotency-Key`.
- **No real KYC or AML.** The API collects and validates holder details, allows
  one account per ID number, and rejects near-duplicates. It does not verify that
  a document is genuine, so someone with several documents or invented numbers
  more than 3 edits apart can still open several accounts.
- **Personal data is stored unencrypted.** Production use needs encryption and
  access controls.
- **Single currency, no authentication.**
- **Schema changes.** Postgres applies `schema.sql` only when the volume is first
  created. A service that evolves would use a migration tool.
- **Credentials.** Those in the Compose files are for local use only.
