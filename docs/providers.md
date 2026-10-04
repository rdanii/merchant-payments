# Payment providers

## Status

| Provider / product | Status | Needed before implementation |
|---|---|---|
| `devsim` | Implemented, **development only** | — (never enable in production; the server refuses to) |
| GoPay (incl. QRIS) | Not implemented | Official merchant API documentation, sandbox credentials, webhook signature scheme, status codes |
| OVO SNAP Dynamic QRIS (`ovo`) | **Implemented, inquiry-only confirmation, not sandbox-verified** | The onboarding confirmations listed [below](#open-items-to-confirm-with-ovo-during-onboarding) |
| OVO callbacks | **Not supported** | OVO's callback verification scheme, which is not publicly documented |
| OVO PayLater | Recognized only as a **funding source** (`OVO_PAYLATER`) of an OVO QRIS payment, when OVO's inquiry reports it | Commercial activation for the merchant; the `allowedSources` bitmap position for PayLater |
| GoPayLater | **Not supported** | Evidence that the merchant's GoPay agreement and API expose GoPayLater. GoPay/QRIS support does not imply it |

Apart from the OVO adapter below, no provider request or response formats,
endpoints, signature schemes or status codes are assumed anywhere in the code.

## OVO SNAP Dynamic QRIS

Sources: OVO Technical Documentation v1.4, the OVO FAQ, and the official
Postman collection "[SNAP] Dynamic QRIS V2"
(https://www.ovo.id/partner-integration/dynamic-qris/). Code:
[internal/provider/ovo/](../internal/provider/ovo/). **Nothing has been run
against OVO's sandbox**; all tests use a local fake OVO server.

### Flow

1. **Token.** `POST /OVOSNAP/v1.0/access-token/b2b` with SHA256withRSA over
   `clientID|X-TIMESTAMP`, hex encoded (as in OVO's Postman script). The token
   is cached. It is renewed 60 seconds before `expiresIn` (documented `900`)
   with a single fetch shared by concurrent callers. After an HTTP 401
   ("Invalid Token (B2B)") it is renewed once and the request retried once.
2. **Create.** `POST /OVOSNAP/v2.0/qr/qr-mpm-generate` with HMAC-SHA512 (key:
   client secret) over
   `POST:<path>:<token>:<lowercase hex SHA-256 of the exact body sent>:<X-TIMESTAMP>`,
   hex encoded.
   - `referenceNo` becomes the provider reference.
   - `qrContent` is returned unchanged as a `QR_STRING` action.
   - `expires_at` is the send time plus `additionalInfo.expiry`. If that is
     missing or outside 75–86400, the configured expiry is used, else 75.
   - The payment becomes `PENDING`. **Creating a QR never confirms anything.**
3. **Confirm.** `POST /OVOSNAP/v2.0/qr/qr-mpm-query`, through
   `POST /api/v1/payments/{id}/inquiry` or the expiry sweep. The report is
   built only from OVO's answer, and the engine applies it only if provider,
   `referenceNo`, `partnerReferenceNo`, amount, currency and status all match.
   The payment, order and audit record are then updated in one transaction.
   Without a callback, checkout clients should poll the inquiry endpoint; the
   sweep is the backstop.

### Status mapping (`latestTransactionStatus`)

| OVO | Payment |
|---|---|
| `00` Payment Success | `PAID` |
| `03` Payment Pending | `PENDING` |
| `06` Transaction Failed | `FAILED` |
| `01` Refund Pending, `04` Refund Success, anything else | Not mapped. The inquiry fails and the payment is unchanged (refunds are out of scope) |

### Funding source (inquiry `transactionSummary` only)

All of `cash`, `points` and `payLater` must be present, well formed, and add
up to the reported amount. Then:

| Summary | `funding_source` |
|---|---|
| only `payLater` > 0 | `OVO_PAYLATER` |
| only `cash` > 0 | `OVO_CASH` |
| points only, any mix, all zero, missing, malformed, not reconciling | `UNKNOWN` |

The requested payment method never sets the funding source. A
`transactionSummary` is accepted at the top level (OVO's field table) or in
`additionalInfo` (OVO's examples). If both are present and differ, the funding
source is `UNKNOWN`.

### Outcome classification

| Situation | Result |
|---|---|
| Create: HTTP 400, 404, 429, or 401 after one token renewal | `ErrRejected`: payment `FAILED` (OVO refused; no QR exists) |
| Create: HTTP 409 duplicate, 5xx, timeout, network error, unexpected body | Outcome unknown: payment stays `CREATED` |
| Inquiry: any non-success, including "Transaction Not Found", refund or undocumented status, malformed or fractional amount | Inquiry fails; payment unchanged |
| Inquiry for a payment without OVO `referenceNo` (QR generation timed out) | Fails locally: OVO requires `originalReferenceNo`. The payment stays `CREATED` and must be reconciled manually with OVO |

### Callbacks

`POST /api/v1/webhooks/ovo` rejects every request with 401 and stores
nothing. OVO's callback (`qr-mpm-notify`) is listed as using a symmetric
signature, but OVO does not publicly document who signs it, with which key,
over which path, or which access token it carries. Do not register a callback
URL with OVO until that is confirmed and implemented; the FAQ lists the
callback URL as optional.

### Open items to confirm with OVO during onboarding

| # | Item | Evidence | Current behavior |
|---|---|---|---|
| 1 | **`partnerReferenceNo` format.** The FAQ says "must be 12 digits", "Only numeric values are allowed". Technical Documentation v1.4 says String(64), "We can generate it based on UUID". The Postman collection uses a 10-digit epoch | Conflicting official sources | Sends the payment's UUID merchant reference (36 characters). **Confirm the accepted production format before go-live** |
| 2 | Signature encoding | OVO's Postman scripts produce hex for both signatures (jsrsasign `signString`, CryptoJS `toString()`); SNAP conventions often use base64 | Hex |
| 3 | `X-PARTNER-ID` value | The Postman collection sends the client ID | Separate `OVO_PARTNER_ID` setting |
| 4 | `additionalInfo.channel` | Documented as "String = 'OVO'"; examples use `ovo` | `OVO` |
| 5 | Which local time zone OVO expects | `X-TIMESTAMP` is documented as the "Client's current local time in yyyy-MM-ddTHH:mm:ssTZD format"; `transactionDate` only as "YYMMDD" | Both use the service's runtime zone (`TZ`): `X-TIMESTAMP` with its offset, `transactionDate` as that zone's calendar date. The zone isn't hard-coded. Confirm with OVO which zone the merchant should run in |
| 6 | `paidTime` format and time zone | The field table says `yyyyMMddHHmmss`; the doc example shows `2023-05-09T18:02:27`; Postman shows `2023-05-02T10:37:53.371+0000` | Only values with an offset become the provider event time; zone-less values are kept in the audit payload but not converted |
| 7 | `amount.currency` in inquiry responses | Documented, but missing from the Postman sample | **Required**: a `PAID` inquiry without currency is rejected and audited, never confirmed |
| 8 | Inquiry success code | Documentation `2005100`; Postman sample `2005300` | Any `200xx00` code with HTTP 200 |
| 9 | `transactionSummary` location | Top level in the field table; inside `additionalInfo` in examples | Both accepted (must agree if both present) |
| 10 | Status of an expired, unpaid QR | Not in OVO's status list | If OVO keeps reporting `03`, the payment stays `PENDING` and its order blocked; nothing is guessed |
| 11 | PayLater `allowedSources` bitmap position and activation | Examples show only cash (1st) and points (2nd) | `OVO_ALLOWED_SOURCES` is passed through as configured |
| 12 | `CHANNEL-ID` value and its presence on the token request | Mandatory in the docs; not sent by the Postman collection | Sent on all requests |
| 13 | Uniqueness scope of `partnerReferenceNo`; FAQ: not supported for idempotency | FAQ | The engine never repeats a QR request for a payment; unknown outcomes are resolved by inquiry |
| 14 | Sandbox access, IP whitelist (production), credential exchange (X.509 and PKCS#1 public keys) | FAQ | — |

## Adding a provider adapter

1. Create `internal/provider/<name>/` implementing `provider.Adapter`
   ([internal/provider/provider.go](../internal/provider/provider.go)), using
   only the provider's official documentation.
2. `SupportsMethod` returns true only for payment methods the merchant's
   contract and the provider's API have been verified to support.
3. `CreatePayment`: return `provider.ErrRejected` only for definitive
   rejections. Return any other error for timeouts, network failures or
   ambiguous responses. The engine then keeps the payment `CREATED` and
   resolves it through inquiry. Never report success from this call.
4. `ParseWebhook`: authenticate the request first and return
   `provider.ErrInvalidSignature` if that fails. Set a stable, unique
   `ProviderEventID`. If the provider has no event id, derive a deterministic
   one from the fields that identify the notification. Set `OccurredAt` only
   from a provider-reported timestamp, never from the local clock.
5. `GetPaymentStatus`: return `provider.ErrInquiryUnsupported` if the provider
   has no status API. The inquiry endpoint then answers `501`. Any other
   error is treated as an unknown outcome, and the payment is left unchanged.
6. Leave `Amount` nil and `Currency` empty when the provider does not report
   them. Never substitute 0 or a default: success reports must carry both.
7. Map provider statuses to `PENDING`, `PAID`, `FAILED` or `EXPIRED`. Leave
   anything ambiguous unmapped so that it is rejected, not guessed.
8. Set `FundingSource` only when the provider explicitly reports it (for
   example a documented field that says balance vs. PayLater). Otherwise leave
   it empty and it is stored as `UNKNOWN`.
9. Set `Payload` to a sanitized copy of the notification: no signatures,
   tokens or credentials, and no customer data beyond what reconciliation
   needs.
10. Read credentials and endpoints from configuration (environment). Do not
   hard-code them. Add the variables to `internal/config`.
11. Register the adapter in `buildProviders` in
   [cmd/server/main.go](../cmd/server/main.go).
12. Add adapter tests against recorded sandbox payloads.
