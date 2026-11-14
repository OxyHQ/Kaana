# Peable invoice draft delivery

This opt-in package connects **Kaana's own Peable application/merchant** to existing Peable invoice, item and event APIs. It does not assume Oxy's central-billing role. Peable already uses Oxy's platform processor/MoR for verified first-party merchants (`Peable` ADR 0015); an app's merchant registration is distinct from a new processor account or legal entity. The existing platform configuration is the source to reuse, not evidence that another seller must be invented.

These slices are source-only and unactivated. No merchant, customer, account, balance, tax setting, invoice or role is created by construction, startup or deployment. There is no serving route, timer, automatic migration or configuration in the production image. The native internal-metered population and PR312's separate commercial serving constructor are unchanged.

## Exact boundaries

- Kaana's ledger owns the settled receivable. `receipts.invoiced` minus existing `refunds.invoiced` is invoiceable; promotional and already-purchased funds are excluded. Account/receipt ownership and unique receipt-to-draft linkage are enforced in PostgreSQL.
- An operator explicitly binds a pre-existing Kaana account to its own-merchant Peable customer. The worker verifies the authenticated merchant's application, owner and environment and the customer's `kaana_account_id` metadata/currency. The expected application is Kaana's reviewed workload identity, never an arbitrary request field or the central merchant's assumed role. `owner_account` here is the seller/application owner, not a replacement payer decision.
- Operator preparation names exact receipt IDs and explicit payment terms (`daysUntilDue`); no billing interval or allowance is guessed. A batch snapshots the exact subtotal, currency exponent and rounding adjustment. Invoice rounding uses the existing Oxy invoice-boundary rule: sum exact decimals, then HALF-UP once to the currency minor unit. The reviewed currency exponent set is USD/EUR/GBP/CHF/CAD/AUD=2 and JPY=0 (`Oxy utils/minorUnits.ts`, `accountInvoicing.service.ts`). Unknown currencies and a subtotal that rounds to zero fail closed; no receipt is claimed in that failed transaction. This does not silently forgive tiny debt.
- During draft delivery the rounding adjustment remains **draft evidence**. The separately enabled close operation below books that adjustment once; issuing never clears debt as paid. Compensation after a snapshot makes its reads/delivery fail for reconciliation, rather than silently changing an issued amount.
- Peable receives a draft with `auto_advance:false`, `send_invoice`, explicit due terms, `pending_invoice_items_behavior:exclude` and automatic tax enabled. Exactly one fixed-amount, non-discountable item is linked directly to that draft. Existing pricing/discounts must already be reflected in the settled ledger; a second discount is not inferred. Final readback rejects added/changed lines. The draft worker **never finalizes, sends, pays or voids** an invoice and supplies no tax rates.

## Durable ambiguity handling

Peable's HTTP idempotency cache lasts 24 hours; it is not permanent business idempotency for invoices/items (`Peable lib/idempotency.ts`). Kaana commits a per-step attempt marker before the first POST and uses a stable idempotency key. A second worker cannot claim that create. On a lost HTTP or DB response, the worker retrieves the merchant/customer-scoped collection and matches the immutable Kaana draft/account metadata plus amount/currency/terms. Duplicate matches, incomplete pagination, mismatched records or no match remain `ErrReconcile`/`ErrMismatch`; **no automatic create retry** occurs, even after the remote cache expires. A crash before the POST may therefore require an operator to reconcile; this is an honest unresolved operation, not permission to duplicate a financial record.

Delivery is bounded to two minutes, each request to 15 seconds, and reconciliation to 1,000 objects. Redirects are refused; response bodies are bounded and never logged/persisted. The only allowed origin is `https://api.peable.to`, wire `2026-11-03`, existing audience `oxy-service:6a2f851751b784a86fd0e958`. The caller injects its own workload token provider; no processor key, shared central role or user credential is accepted/configured by this package.

`ReconcileEvent` accepts only an event ID as a wakeup and retrieves the authoritative event through the same authenticated merchant connection. Only draft-related events can wake delivery. It trusts neither an incoming webhook body nor a claimed amount/payment; paid/credit/funding events have no action here. There is intentionally no public webhook handler. Event redelivery/restart converges on the same stored draft. A production handler must enforce the existing Peable webhook authentication/rate limits when wired; payment funding/reversal reconciliation is separate work.

## Offline migration and permissions

Provision NOLOGIN groups `kaana_peable_billing_admin` and `kaana_peable_billing_runtime` separately. Run `Store.Migrate` explicitly as the existing **kaana_billing_migrator**, after the billing ledger migration. It owns a new `kaana_peable_billing` schema and a checksummed migration ledger; no existing immutable migration is changed. No group membership is granted automatically.

The admin can bind customers and prepare batches. Delivery runtime can only read/check a draft, claim a fixed step and record its external reference. Neither can write tables directly. The delivery group cannot create/fund/configure billing accounts, reserve/settle usage, migrate or change customer bindings. SQL functions have a fixed search path; PUBLIC access is revoked. Serving never receives the migrator or draft-admin group.

## Validation and remaining work

Directed Go/HTTP tests exercise exact request semantics, own-merchant refusal, redirects, response loss and DB-receipt loss, duplicate events, absent-result reconciliation, paid/manual invoice refusal and no extra financial endpoint. Real PostgreSQL tests exercise amount partitions, aggregate fractional rounding, compensation conflicts, duplicate/foreign receipts, concurrent one-shot claims, checksums, role denial and worker restart with persisted state.

Still required before real commercial billing: actual own-app merchant binding under the already-configured Oxy seller; reviewed reuse of that seller's fiscal settings (Peable settings/numbering are merchant-scoped, not automatically inherited); explicit customer enrollment/terms; verified signed payment/funding/reversal reconciliation; collection/credit notes and operational alerts. This package adds no production permission, funds, seller choice or financial activation.

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
No timer, receipt-selection cadence, webhook listener or deployment
is enabled here; close requires its separate explicit configuration and action. A future scheduler may claim only explicitly prepared work and
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


## Reviewed issue and durable close (additional opt-in)

This source adds a separate `finalizationEnabled:true` configuration gate and
NOLOGIN `kaana_peable_billing_closer` group. Neither is provisioned or enabled by
this repository. Existing delivery configuration and grants remain insufficient
to issue invoices. `migrate-close` explicitly applies checksummed migration0002
under `kaana_billing_migrator`, after billing and draft migrations. It adds the
attempt/evidence table and refund guard and extends the balanced journal's kind
set; old migration bytes remain unchanged. `migrate` alone does not install it.

The reviewed operator sequence is `review-close` with a draft ID, followed by
`close` with that draft ID, a durable operation key and the exact returned
`revision`. The review uses Peable's opt-in retrieve. A close accepts only the
same own-merchant/customer/line/terms and commits its local claim BEFORE POST.
The account lock is the same lock used by refunds; it rechecks receipt net under
that lock. Only one claim can issue. No constructor, serving request, recovery
loop or delivery action invokes this sequence automatically. Peable finalization
can number and issue an invoice, apply its existing customer balance, and queue
mail according to the already-reviewed merchant settings. Real execution of this
operation therefore requires review of the exact merchant, customer, receipt
batch, terms and revision; this source does not execute it or invent those terms.

A lost HTTP/DB response, crash, 409 or missing receipt never permits another POST
or a changed operation key. Even a typed conflict may mean another actor already
issued the invoice. Recovery retrieves its `accepted_finalization_revision` and
requires exact equality; equal totals/customer are insufficient. Peable's matching
opt-in contract must be published/deployed first. A missing or different proof
remains unresolved. No automatic fence-release/reset operation is exposed.

Local close stores bounded financial evidence (invoice number/status, accepted
revision, base/tax/total, due/paid and balance snapshot, digest) and posts the
previously reviewed aggregate rounding adjustment once. It adjusts `invoiced`
only by that exact adjustment; invoice issue does not collect the usage debt.
Tax and customer-balance effects remain Peable evidence, not fabricated Kaana
funding or duplicated tax journals. Inference budget usage remains its original
exact rated amount. Positive, negative and zero rounding have directed controls.

Any invoice-credit compensation for a receipt under an attempted or closed
invoice is refused before the refund inserts or alters balances. Purchased or
promotional-only compensations remain outside this guard. This prevents silent
post-issue net changes; it does not implement credit notes, forgiveness, collection
or an operator override. Resolving a refused/ambiguous issue, subsequent credit
notes and payment reconciliation remain explicit follow-up operations. No
unresolved attempt expires into permission to issue again.

The closer can only read the draft and read/claim/record close evidence. It cannot
prepare/bind customers, create draft invoices, directly edit billing tables,
execute billing-admin functions, or delegate grants. Current/session permissions
are checked against exact function signatures. The CLI refuses a close operation
before opening DB or identity unless its separately hashed configuration enables
finalization. There is no production config or financial activation in this PR.
