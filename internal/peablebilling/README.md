# Peable invoice draft delivery

This opt-in package connects **Kaana's own Peable application/merchant** to existing Peable invoice, item and event APIs. It does not assume Oxy's central-billing role. Peable already uses Oxy's platform processor/MoR for verified first-party merchants (`Peable` ADR 0015); an app's merchant registration is distinct from a new processor account or legal entity. The existing platform configuration is the source to reuse, not evidence that another seller must be invented.

This slice is source-only and unactivated. No merchant, customer, account, balance, tax setting, invoice or role is created by construction, startup or deployment. There is no serving route, timer, automatic migration or configuration in the production image. The native internal-metered population and PR312's separate commercial serving constructor are unchanged.

## Exact boundaries

- Kaana's ledger owns the settled receivable. `receipts.invoiced` minus existing `refunds.invoiced` is invoiceable; promotional and already-purchased funds are excluded. Account/receipt ownership and unique receipt-to-draft linkage are enforced in PostgreSQL.
- An operator explicitly binds a pre-existing Kaana account to its own-merchant Peable customer. The worker verifies the authenticated merchant's application, owner and environment and the customer's `kaana_account_id` metadata/currency. The expected application is Kaana's reviewed workload identity, never an arbitrary request field or the central merchant's assumed role. `owner_account` here is the seller/application owner, not a replacement payer decision.
- Operator preparation names exact receipt IDs and explicit payment terms (`daysUntilDue`); no billing interval or allowance is guessed. A batch snapshots the exact subtotal, currency exponent and rounding adjustment. Invoice rounding uses the existing Oxy invoice-boundary rule: sum exact decimals, then HALF-UP once to the currency minor unit. The reviewed currency exponent set is USD/EUR/GBP/CHF/CAD/AUD=2 and JPY=0 (`Oxy utils/minorUnits.ts`, `accountInvoicing.service.ts`). Unknown currencies and a subtotal that rounds to zero fail closed; no receipt is claimed in that failed transaction. This does not silently forgive tiny debt.
- The rounding adjustment remains **draft evidence**, not a posted financial adjustment. Finalization/collection and booking its adjustment require the next invoice-close slice; no debt is cleared or money moved here. Compensation after a snapshot makes its reads/delivery fail for reconciliation, rather than silently changing an issued amount.
- Peable receives a draft with `auto_advance:false`, `send_invoice`, explicit due terms, `pending_invoice_items_behavior:exclude` and automatic tax enabled. Exactly one fixed-amount, non-discountable item is linked directly to that draft. Existing pricing/discounts must already be reflected in the settled ledger; a second discount is not inferred. Final readback rejects added/changed lines. This package **never finalizes, sends, pays or voids** an invoice and supplies no tax rates.

## Durable ambiguity handling

Peable's HTTP idempotency cache lasts 24 hours; it is not permanent business idempotency for invoices/items (`Peable lib/idempotency.ts`). Kaana commits a per-step attempt marker before the first POST and uses a stable idempotency key. A second worker cannot claim that create. On a lost HTTP or DB response, the worker retrieves the merchant/customer-scoped collection and matches the immutable Kaana draft/account metadata plus amount/currency/terms. Duplicate matches, incomplete pagination, mismatched records or no match remain `ErrReconcile`/`ErrMismatch`; **no automatic create retry** occurs, even after the remote cache expires. A crash before the POST may therefore require an operator to reconcile; this is an honest unresolved operation, not permission to duplicate a financial record.

Delivery is bounded to two minutes, each request to 15 seconds, and reconciliation to 1,000 objects. Redirects are refused; response bodies are bounded and never logged/persisted. The only allowed origin is `https://api.peable.to`, wire `2026-11-03`, existing audience `oxy-service:6a2f851751b784a86fd0e958`. The caller injects its own workload token provider; no processor key, shared central role or user credential is accepted/configured by this package.

`ReconcileEvent` accepts only an event ID as a wakeup and retrieves the authoritative event through the same authenticated merchant connection. Only draft-related events can wake delivery. It trusts neither an incoming webhook body nor a claimed amount/payment; paid/credit/funding events have no action here. There is intentionally no public webhook handler. Event redelivery/restart converges on the same stored draft. A production handler must enforce the existing Peable webhook authentication/rate limits when wired; payment funding/reversal reconciliation is separate work.

## Offline migration and permissions

Provision NOLOGIN groups `kaana_peable_billing_admin` and `kaana_peable_billing_runtime` separately. Run `Store.Migrate` explicitly as the existing **kaana_billing_migrator**, after the billing ledger migration. It owns a new `kaana_peable_billing` schema and a checksummed migration ledger; no existing immutable migration is changed. No group membership is granted automatically.

The admin can bind customers and prepare batches. Delivery runtime can only read/check a draft, claim a fixed step and record its external reference. Neither can write tables directly. The delivery group cannot create/fund/configure billing accounts, reserve/settle usage, migrate or change customer bindings. SQL functions have a fixed search path; PUBLIC access is revoked. Serving never receives the migrator or draft-admin group.

## Validation and remaining work

Directed Go/HTTP tests exercise exact request semantics, own-merchant refusal, redirects, response loss and DB-receipt loss, duplicate events, absent-result reconciliation, paid/manual invoice refusal and no extra financial endpoint. Real PostgreSQL tests exercise amount partitions, aggregate fractional rounding, compensation conflicts, duplicate/foreign receipts, concurrent one-shot claims, checksums, role denial and worker restart with persisted state.

Still required before real commercial billing: actual own-app merchant binding under the already-configured Oxy seller; reviewed reuse of that seller's fiscal settings (Peable settings/numbering are merchant-scoped, not automatically inherited); explicit customer enrollment/terms; invoice close and durable rounding postings; signed payment/funding/reversal reconciliation; collection/credit notes and operational alerts. This package adds no production permission, funds, seller choice or financial activation.

## Explicit operator (source-only)

The existing `kaana-native` image exposes `peable-billing --config-file <file>
--config-sha256 <reviewed-byte-hash> [--operation-file <private-file>]`. Without an
operation file it reads stdin. The configuration is a bounded, strictly decoded
JSON object with `enabled:true`, `applicationId`, `ownerAccountId`, `merchantId`
and `environment`. There is no built-in merchant, central-billing role, default
opt-in or environment fallback. The operation document is also strict and rejects
duplicate fields, unrelated action fields, absent terms and repeated receipts.
Configuration/hash/action validation precedes database or workload identity use.
No configuration file or production role grant is shipped by this change.

Supported operation documents are deliberately explicit:

- `{"action":"migrate"}` applies only this package's checksummed schema after
  billing migration. It does not migrate/enroll native authority or billing accounts.
- `{"action":"bind","accountId":"<reviewed-account>","customerId":"<reviewed-customer>"}`
  records a previously reviewed own-merchant customer/account association. The
  merchant is taken only from the reviewed configuration. It does not create a
  customer or fetch/copy customer profile information. Confirm the existing
  customer's account metadata and currency before this immutable binding.
- `{"action":"prepare","accountId":"<account>","key":"<operation-key>","receiptIds":["<receipt>"],"daysUntilDue":30}`
  prepares exactly those receipts under the explicitly supplied terms (30 here is
  an example, never a default). Seller mismatch rolls back receipt allocation in
  the same PostgreSQL transaction.
- `{"action":"inspect","draftId":"<draft>"}` reads the scoped draft without
  opening AWS identity or calling Peable. It refuses compensation conflicts.
- `{"action":"deliver","draftId":"<draft>"}` runs the existing bounded worker
  with Kaana's own workload attestation and Peable audience. It verifies the actual
  merchant/customer before the first external create. Ambiguous attempts resume
  only through the existing durable reconciliation path.
- `{"action":"reconcile-event","eventId":"<event>"}` retrieves and reconciles
  the authenticated draft-related event. It is an explicit operator action, not
  an unsigned public webhook endpoint.

`DATABASE_URL` must identify a separately provisioned least-privilege operator
login, with SET-only membership in exactly one role for the chosen action:
`kaana_billing_migrator` for migrate, `kaana_peable_billing_admin` for bind/prepare,
or `kaana_peable_billing_runtime` for inspect/deliver/reconcile-event. Pool creation
checks the effective role, schema/function privileges, raw financial DML denial,
NOLOGIN group and absence of ADMIN OPTION or other memberships. The inference
login is not repurposed as an operator. Production TLS follows the existing
verified PostgreSQL boundary. No role is created or granted by this command.

The process deadline is three minutes and delivery retains its two-minute bound.
Outputs contain the action and draft/invoice/item references only. Errors never
expose database URLs, upstream bodies or credentials. Successful output means the
named operation completed; it does **not** mean an invoice was finalized, sent,
paid, or posted as collected. Restart uses the same operation key/draft; changed
keys must never be used to evade an ambiguous attempt.

This operator is independent of PR312's serving recovery: native recovery rates
and settles usage; prepared invoice batches are consumed outside inference latency.
No timer, receipt-selection cadence, financial close, webhook listener or deployment
is enabled here. A future scheduler may claim only explicitly prepared work and
must retain the same claim-before-POST and reconciliation guarantees.

The confirmed production mapping reuses Oxy's existing Peable platform/MoR. Its
central merchant already has approved platform tax settings; Kaana's own merchant
registration is still a separate prerequisite. Peable's existing tax dashboard
requires an Oxy user with current `billing:manage` permission on Kaana to apply and
approve scoped settings; the user may be the owner or an authorized application
member. The workload cannot approve in that person's place, and neither approval
identity/timestamps nor absent registrations are copied or invented. No new legal
entity or processor account is implied. Existing merchant-specific default invoice
series are allocated by Peable; their absence before first use is not a missing
platform seller.
