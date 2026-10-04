# merchant-payments

Payment backend for a merchant: orders, payments, provider confirmation via
webhooks, idempotency, and an append-only audit trail for reconciliation.

> **Provider status:** OVO SNAP Dynamic QRIS (`provider: "ovo"`) is
> implemented with **inquiry-only confirmation**. OVO callbacks are not
> supported yet. It has **not been verified against OVO's sandbox**, and
> several provider details still need OVO's confirmation during onboarding;
> see [docs/providers.md](docs/providers.md#ovo-snap-dynamic-qris). GoPay,
> GoPayLater and any other provider are not implemented. `devsim` is a
> development-only simulator that the server refuses to enable in production.

## Scope and current limitations

Read this before deploying anything.

- **One production provider adapter, not yet sandbox-verified.** OVO SNAP
  Dynamic QRIS confirms payments through Transaction Inquiry only. Callbacks
  are unsupported, and the `partnerReferenceNo` format must be confirmed with
  OVO before production. OVO PayLater is recognized as a *funding source*
  (`OVO_PAYLATER`) only when OVO's inquiry reports it. GoPay and GoPayLater are
  not implemented. See [docs/providers.md](docs/providers.md).
- **The merchant API is unauthenticated.** `/api/v1/orders*` and
  `/api/v1/payments*` accept any caller. Until merchant authentication exists,
  run the service only on a private network, or behind a gateway that
  authenticates callers. Expose only `POST /api/v1/webhooks/{provider}`
  publicly; webhooks are authenticated by each provider adapter.
- **Settlement and full reconciliation are later-phase scope.** There is no
  settlement data (settlement reference, settled time, fees, net amount) and
  no settlement processing. There is no reconciliation workflow or alerting.
  This phase provides the inputs for them: the append-only `payment_events`
  audit trail (including rejected provider reports, with what the provider
  claimed), payment status inquiry, and provider references.
- **A late PAID after local FAILED/EXPIRED is a reconciliation case.**
  `FAILED` and `EXPIRED` are terminal. If a provider later reports `PAID` for
  such a payment, for example after a local expiry or a provider-side delay,
  the report is rejected. It is stored in `payment_events` with
  `outcome = 'REJECTED'` and `reported_status = 'PAID'`. Money may have moved
  while the payment shows as not paid, so these records must be reviewed. Nothing
  alerts on them yet:

  ```sql
  SELECT * FROM payment_events WHERE outcome = 'REJECTED' AND reported_status = 'PAID';
  ```

## Payment flow

```
POST /orders            order CREATED
POST /orders/{id}/payments
                        payment CREATED ──provider accepts──▶ PENDING
                                        ──provider rejects──▶ FAILED
                                        ──timeout/unknown───▶ stays CREATED (resolve by webhook or inquiry)
POST /webhooks/{provider}            or
POST /payments/{id}/inquiry
  (verified PAID report) payment PENDING ──▶ PAID  ┐ one transaction,
                         order   CREATED ──▶ PAID  ┘ plus an audit event
expiry sweep (background)
  provider expires_at + grace passed: payment CREATED/PENDING ──▶ EXPIRED
```

Payment statuses: `CREATED → PENDING | PAID | FAILED | EXPIRED`,
`PENDING → PAID | FAILED | EXPIRED`. `PAID`, `FAILED` and `EXPIRED` are
terminal. Refunds or reconciliation corrections must be added as explicit new
transitions ([internal/domain/status.go](internal/domain/status.go)).

## Business invariants and where they are enforced

| Invariant | Enforcement |
|---|---|
| Creating a payment never makes it PAID | `domain.NewPayment` creates `CREATED`; provider acceptance only moves it to `PENDING` |
| PAID only after a valid provider confirmation | `Payment.ApplyReport` is the only code path that sets `PAID`; reports come from authenticated webhooks or provider inquiry |
| Provider, merchant reference, provider reference, amount, currency and status are validated before success | `Payment.verifyReport` |
| `funding_source` is never inferred | Starts `UNKNOWN`; only set from the provider's report; unrecognized or missing values become `UNKNOWN` (`domain.NormalizeFundingSource`) |
| No transitions out of PAID | State machine in `domain/status.go`; DB check `paid_at` ⇔ `PAID` |
| Duplicate webhooks are idempotent | Unique index on `payment_events (provider, provider_event_id)`; duplicates return `{"status":"duplicate"}` and change nothing |
| Payment and order updates stay consistent | `Service.applyReport` updates payment, order and audit event in one DB transaction (locks order, then payment) |
| At most one in-flight and one successful payment per order | Partial unique indexes `payments_one_active_per_order`, `payments_one_paid_per_order` |
| Payment creation is idempotent | Required `Idempotency-Key` header; unique `(order_id, idempotency_key)` |
| Audit trail | `payment_events` is append-only (DB trigger). Authenticated reports that are rejected are stored too, with what the provider claimed: validation failures, a missing event id, and storage-constraint rejections (recorded in a separate transaction after the rollback). Webhooks that fail authentication or parsing are logged at WARN but not stored, because they cannot be attributed to the provider |
| Provider vs. local time | `payment_events.provider_occurred_at` holds the provider-reported event time (NULL if not reported); `created_at` and `paid_at` are local processing times |
| Reported amounts | `payment_events.reported_amount` is NULL when the provider did not report an amount, distinct from an explicit `0` |
| No secrets in logs | Only method/path/status are logged per request; `Config` redacts itself; webhook headers and bodies are never logged; adapters must sanitize payloads before they are stored |

## Layout

```
cmd/server/                   entry point, wiring, provider registration
internal/config/              environment configuration
internal/domain/              orders, payments, state machine, validation (no I/O)
internal/payment/             payment engine: use cases and transactions
internal/payment/paymenttest/ in-memory store for tests only
internal/provider/            provider adapter interface + registry
internal/provider/devsim/     DEVELOPMENT-ONLY simulator
internal/provider/providertest/ scriptable fake for tests only
internal/store/postgres/      PostgreSQL repository + embedded migrations
internal/httpapi/             REST handlers, validation, error mapping
internal/requestid/           request ID in contexts and log records
```

## API

All bodies are JSON. Amounts are integers in the ISO 4217 minor unit of the
currency (for IDR, whole rupiah). Unknown JSON fields are rejected.

| Method | Path | Notes |
|---|---|---|
| `POST` | `/api/v1/orders` | `{"merchant_reference","amount","currency","description"?}` → `201`. Retrying with the same `merchant_reference` and identical data returns the existing order with `200`; reusing it with different data gives `409 DUPLICATE_ORDER` |
| `GET` | `/api/v1/orders/{id}` | |
| `POST` | `/api/v1/orders/{id}/payments` | Header `Idempotency-Key` (8–128 chars) required. `{"provider","payment_method"}` → `201` (new) or `200` (replay). Amount and currency come from the order |
| `GET` | `/api/v1/payments/{id}` | |
| `POST` | `/api/v1/payments/{id}/inquiry` | No body. Asks the provider for the payment's status and applies it with the same validation as webhooks → `200` with the payment. `502 PROVIDER_OUTCOME_UNKNOWN` if the provider call fails (payment unchanged), `501 INQUIRY_UNSUPPORTED` if the adapter cannot inquire (`devsim` cannot), `422` if the provider's answer is rejected |
| `POST` | `/api/v1/webhooks/{provider}` | Authenticated by the provider's adapter. `200 processed`/`duplicate`, `401` bad signature, `422` rejected report (still audited), `404` unknown provider |
| `GET` | `/healthz`, `/readyz` | Liveness; readiness pings the database |

`payment_method` is one of `QRIS`, `EWALLET`, `PAYLATER`; a provider adapter
decides which of them it supports.

Errors use one envelope:

```json
{"error": {"code": "VALIDATION_FAILED", "message": "...", "fields": {"amount": "..."}}}
```

If the provider call times out or fails in an unknown way, payment creation
returns `502 PROVIDER_OUTCOME_UNKNOWN` with a `payment_id`. The payment stays
`CREATED` and changes only when a provider webhook arrives or
`POST /api/v1/payments/{id}/inquiry` succeeds. Nothing calls inquiry
automatically. Such a payment has no provider expiry, so it is never expired
locally, because it may have succeeded at the provider.

Every response carries an `X-Request-ID` header, and log records written while
handling the request include the same `request_id`.

### Local expiry

A background sweep (every `EXPIRY_SWEEP_INTERVAL`) handles `CREATED`/`PENDING`
payments whose provider-issued `expires_at` is more than
`PAYMENT_EXPIRY_GRACE` in the past. **It asks the provider first**, using the
same path as the inquiry endpoint. No database lock is held during the
provider call.

| Inquiry result | Action |
|---|---|
| The provider answers (`PAID`, `FAILED`, `PENDING`, ...) | The answer is applied with the usual validation. The payment is **never** expired locally; a payment still `PENDING` is asked about again on the next sweep |
| The adapter has no inquiry (`devsim`) | Expired locally, with a `PAYMENT_EXPIRED_LOCALLY` audit event |
| The inquiry fails, or its answer is rejected | Nothing changes; retried on the next sweep. An unknown outcome is never turned into `EXPIRED` or `FAILED` |

Expiring a payment frees the order for a new attempt, because only one
in-flight payment per order is allowed. The sweep pages through every overdue
payment, so unresolved ones cannot starve newer ones. Several instances may run
the sweep safely. For a provider that answers inquiries, an order whose
provider keeps reporting `PENDING` stays blocked until the provider resolves
it. Nothing is guessed. See also the late-PAID note under
[Scope and current limitations](#scope-and-current-limitations).

### Not implemented yet

- `GET /api/v1/payments/{id}/events`: the data is in `payment_events`, but
  there is no read path in the repository or the API yet.
- `POST /api/v1/payments/{id}/refund`: needs a new adapter method and explicit
  `PAID → REFUNDED` transitions.

## Configuration

| Variable | Default | |
|---|---|---|
| `DATABASE_URL` | — (required) | Secret. Never logged. A URL or a keyword string such as `host=db port=5432 sslmode=disable`; missing fields fall back to `PGUSER`, `PGPASSWORD`, `PGDATABASE` |
| `APP_ENV` | `production` | `production` or `development` |
| `HTTP_ADDR` | `:8080` | |
| `AUTO_MIGRATE` | `false` | Apply embedded migrations at startup |
| `PROVIDER_TIMEOUT` | `15s` | Upper bound for every provider call |
| `SHUTDOWN_TIMEOUT` | `20s` | Graceful shutdown |
| `EXPIRY_SWEEP_INTERVAL` | `1m` | How often overdue payments are expired |
| `PAYMENT_EXPIRY_GRACE` | `5m` | Time after the provider's `expires_at` before a payment is expired locally |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN`, `ERROR` |
| `DEVSIM_ENABLED` | `false` | Development simulator; rejected when `APP_ENV=production` |
| `DEVSIM_WEBHOOK_SECRET` | — | Secret, ≥16 chars, required with `DEVSIM_ENABLED` |
| `OVO_ENABLED` | `false` | Enables the OVO SNAP Dynamic QRIS adapter. All `OVO_*` values below come from OVO merchant onboarding and are required when enabled, except `OVO_QR_EXPIRY_SECONDS` |
| `OVO_BASE_URL` | — | OVO host (staging or production), e.g. as listed in OVO's technical documentation. Must be `https` (http only in development) |
| `OVO_CLIENT_ID` | — | `X-CLIENT-KEY` |
| `OVO_CLIENT_SECRET` | — | **Secret.** HMAC-SHA512 signing key |
| `OVO_PRIVATE_KEY_FILE` | — | Path to the RSA private key (PEM, PKCS#1 or PKCS#8) for the B2B token signature. **The file is secret** |
| `OVO_PARTNER_ID` | — | `X-PARTNER-ID` (OVO's Postman collection uses the client ID here; confirm with OVO) |
| `OVO_CHANNEL_ID` | — | `CHANNEL-ID` |
| `OVO_MERCHANT_ID`, `OVO_TERMINAL_ID` | — | `merchantId`, `terminalId` |
| `OVO_DEVICE_ID` | — | `additionalInfo.deviceId` (documented as a static string) |
| `OVO_ALLOWED_SOURCES` | — | `allowedSources` bitmap, e.g. `100` = cash only. Depends on the merchant's OVO agreement |
| `OVO_QR_EXPIRY_SECONDS` | unset | 75–86400. Unset lets OVO apply its default of 75 seconds |
| `TZ` | system zone | Standard runtime time zone (e.g. `Asia/Jakarta`). OVO requires `X-TIMESTAMP` in the client's local time, and `transactionDate` uses the same zone. The zone database is embedded in the binary. With OVO enabled, an unknown name is rejected at startup, because Go would otherwise silently use UTC. In a container without `TZ` the zone is UTC |

## Local setup

Requirements: Go 1.25+, and either Docker or a local PostgreSQL 14+.

### With Docker Compose (local development only)

`docker-compose.yml` is for local development only. It runs with
`APP_ENV=development` and binds ports to localhost. It passes database
credentials as `PGUSER`/`PGPASSWORD`/`PGDATABASE`, so passwords need no URL
escaping. The app's container healthcheck runs `/server healthcheck`, which
calls `/healthz`.

```sh
cp .env.example .env          # local-only values; edit the secrets
docker compose up --build
curl -s localhost:8080/readyz
```

### Without Docker

```sh
createdb payments
export DATABASE_URL='postgres://USER:PASSWORD@localhost:5432/payments?sslmode=disable'
APP_ENV=development AUTO_MIGRATE=true \
DEVSIM_ENABLED=true DEVSIM_WEBHOOK_SECRET=change-me-local-dev-secret \
go run ./cmd/server
```

### Walk through the flow with the simulator

The simulator never confirms anything on its own. You play the provider by
posting a webhook signed with `DEVSIM_WEBHOOK_SECRET` (HMAC-SHA256 of the raw
body, hex, in `X-Devsim-Signature`). The commands below use `jq` and `openssl`.

```sh
B=http://localhost:8080
SECRET=change-me-local-dev-secret

OID=$(curl -s -X POST $B/api/v1/orders -H 'Content-Type: application/json' \
  -d '{"merchant_reference":"INV-1001","amount":150000,"currency":"IDR"}' | jq -r .id)

PAY=$(curl -s -X POST $B/api/v1/orders/$OID/payments -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: checkout-INV-1001-1' -d '{"provider":"devsim","payment_method":"QRIS"}')
echo "$PAY"    # status PENDING, funding_source UNKNOWN
PID=$(echo "$PAY" | jq -r .id); PREF=$(echo "$PAY" | jq -r .provider_reference)

BODY="{\"event_id\":\"evt-0001\",\"provider_reference\":\"$PREF\",\"merchant_reference\":\"$PID\",\"status\":\"PAID\",\"amount\":150000,\"currency\":\"IDR\"}"
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | sed 's/^.* //')
curl -s -X POST $B/api/v1/webhooks/devsim -H "X-Devsim-Signature: $SIG" -d "$BODY"   # processed / APPLIED
curl -s -X POST $B/api/v1/webhooks/devsim -H "X-Devsim-Signature: $SIG" -d "$BODY"   # duplicate

curl -s $B/api/v1/payments/$PID   # PAID
curl -s $B/api/v1/orders/$OID     # PAID
curl -s -X POST $B/api/v1/payments/$PID/inquiry   # 501 INQUIRY_UNSUPPORTED: the simulator has no inquiry
```

Simulator webhook fields: `event_id`, `provider_reference`,
`merchant_reference`, `status` (required), and optional `amount`, `currency`,
`funding_source`, `occurred_at` (RFC 3339 provider event time).

## Tests

```sh
go test ./...
```

Unit and HTTP tests run without a database. The PostgreSQL integration tests
cover constraints, transactions, concurrent duplicate webhooks, expiry,
migrations and audit columns. They need `TEST_DATABASE_URL` pointing at a
**disposable** database, because they truncate all tables. Without it they are
**skipped**, and `go test` still prints `ok` for the package. Set
`REQUIRE_DB_TESTS=1` to make a missing database fail instead.

```sh
createdb payments_test
REQUIRE_DB_TESTS=1 \
TEST_DATABASE_URL='postgres://USER:PASSWORD@localhost:5432/payments_test?sslmode=disable' \
go test -race -count=1 ./...     # -race needs cgo (a C compiler)
```

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) runs gofmt, `go vet`
and `go test -race` against a PostgreSQL 16 service with
`REQUIRE_DB_TESTS=1`, so skipped integration tests fail the build.
