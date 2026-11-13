# ADR 0010 — Kaana owns inference economics; Oxy keeps identity

- **Decision accepted:** owner direction on 2026-11-12, explicitly confirmed
  and narrowed on 2026-11-13: Kaana processes inference, authorizes spending
  and invoices; Oxy supplies identity only.
- **Implementation is staged.** Contract, admission implementation and
  production activation are separate states. No live financial migration is
  asserted by this ADR.
- Supersedes the economic-authority portions of ADRs 0001 and 0002 and the
  former rule that Kaana may never price customer usage. Their accounting,
  identity, credential-custody and execution invariants remain requirements.
- Counterpart: [Oxy ADR 0065](https://github.com/OxyHQ/oxy/blob/main/docs/adr/0065-kaana-economic-authority-oxy-identity.md).

## Decision

Inference content goes directly from the calling product to Kaana. Stateful
product work belongs to Gemies, including the Alia gemie; conversations,
tools, memory and approvals remain outside Kaana.

Oxy verifies accounts, applications, credentials, workload bindings and person
sessions. It does not select an inference policy, model, funding class, price,
payer or spending permission in the successor identity operation. Kaana owns
its catalogue, power levels, routing and economic policy, admission, exact
rating, balances, holds, settlement, receipts, payments and invoices. Oxy
Console may present those functions through Kaana's API; that does not make
Oxy the financial authority. Gemies product credits are a separate ledger.

An identity assertion is not a spend grant. The canonical contract in
`packages/contracts/src/internal/identity.ts` binds its exact identity-request
bytes, request ID, audience and bounded expiry under a separate signature
domain. It contains no inference content or economic facts. Own workloads do
not acquire artificial scopes; external credentials retain their verified
scopes. User attribution on a workload or key is not proof of a person or
permission to spend from that person's account.

The session extension distinguishes verified person authority from workload
identity and application attribution. A public client ID does not authenticate
an app. The application owner and verified subject/principal are separate
facts; Kaana chooses an authorized payer under its own policy. An authenticated
session cannot inherit a service-workload enrollment merely by naming the same
application. All currently supported credential lanes must be classified
before financial authority is selected; an authentication error never triggers
fallback to an older economic endpoint.

## Source and production status, 2026-11-13

| Capability | Evidence and state |
|---|---|
| Identity-only service/machine contract | Merged in #302 (`6b8a1a0`), contracts 8.13; Zod source, generated descriptor and Go validation. This does not deploy an Oxy endpoint or activate native admission. |
| Session identity contract and Oxy verifier/emitter | Contract merged in #304 (`feb1f11`) and published as 8.14. Oxy verifier/emitter remains a tested candidate; no production endpoint activation claimed. |
| Kaana-owned internal metered admission | `internal/nativeadmission` candidate implements reviewed policy, exact pricing, PostgreSQL capacity/idempotency and settlement work. It is not commercial customer balance or invoice delivery, and source presence does not enroll any workload. |
| Current public execution | Existing Oxy grants and settlement delivery remain authoritative until each population's verified cutover. |
| Retention and observability | Already implemented in #299: migration 0024 and bounded purge tooling, signed metrics and CloudWatch EMF. Operational scheduling/alarms and financial-record retention still need their own configuration. |
| Commercial seller | The approved read-only Peable lookup found zero production merchants for Kaana's exact application. No merchant, fiscal settings or payment setup was created. |

The Gemies backend retains the former Alia immutable application ID in
`packages/server/src/identity.ts`. A read-only production check confirmed its
active workload binding and matched the deployed Oxy policy source. Source and
production agree; the newly registered Gemies brand application is a different
principal. Enroll only the verified workload with reviewed terms, never an app
inferred from its name. The old Oxy #2256 output-cap proposal is closed,
unmerged and superseded; it is not an approved policy dependency.

## One admission, one financial authority

The durable admission fixes its authority, identity/idempotency scope, body
digest and immutable policy/price version before provider execution. Existing
Oxy-issued grants remain Oxy-owned through settlement, expiry and historical
reads. Kaana-owned admissions settle only in Kaana. A timeout, cancellation,
unknown outcome or retry cannot switch authority or execute the request again.

Cutover must import or consult existing idempotency bindings before enabling
new admissions. Oxy's caller-supplied-key namespace includes the credential ID;
its expiry of a hold or concurrency lease does not erase the retry binding.
Preserve that uniqueness across the boundary. A durable exact-principal workload
fence stops new Oxy admissions before the final zero-unresolved-state export.
Unknown historical body digests remain unknown: imported tombstones always refuse
new execution under that key, regardless of the presented body. A known digest
only establishes body equivalence when its canonicalization domain matches. Late Oxy settlements continue through the old outbox; do not
rewrite them as Kaana charges or delete them to make reconciliation pass.

A first internal-metered population can move admission without transferring
customer funds. This still requires a reviewed exact-principal policy,
capacity/funding/price bounds, a complete legacy-key fence, atomic durable
settlement, receipt correlation and production readback. It does not by itself
complete commercial billing or the broader financial migration.

## Commercial implementation and migration inventory

These are concrete remaining deliverables, not unresolved ownership questions:

| Work | Existing source and required completion |
|---|---|
| Account/currency funding and journals | Oxy `account_balances` and `billing_ledger_*`, with nearest active ancestor billing profiles. Build Kaana-owned exact-decimal balances and append-only journals; do not copy a shared funded balance per app. |
| Holds, spending policy and settlement | Oxy `usage_reservations`, per-app spending limits and `inferenceLedger.service.ts`. Implement transactional reserve/settle/refund, expiry, funding restrictions and concurrent-budget enforcement in Kaana. Preserve immutable prices and body/idempotency conflicts. |
| Funded-population inventory | A bounded read-only census of nonzero purchased/promotional/invoiced buckets, expiries, open holds, debts, shared funding accounts and processor references is required before choosing a transfer boundary. No such production census or monetary transfer is claimed here. |
| Historical receipts and invoices | Keep original Oxy authority/read access and invoice associations. Reconcile old holds/outbox before a uniquely keyed transfer; never make one receipt billable in both systems. The public Kaana receipt relay currently reads Oxy's existing records, so changing transport alone needs no duplicate receipt ledger. |
| Seller and payments | Create Kaana's Peable merchant only after the owner identifies its legal entity and approved fiscal configuration. Oxy inference top-ups still call the actual Stripe SDK; Oxy One's native Peable merchant is a separate product and must not be borrowed. Implement signed/idempotent payment webhooks, recharge, refunds, disputes and processor reconciliation in Kaana. |
| Invoices and commercial terms | Implement invoice close, tax configuration, credit notes and receipt linkage. Preserve existing reviewed allowances, promotions, limits and discounts until a concrete replacement is approved; ownership migration does not invent prices. |
| Deployment permissions | Catalogue list prices do not establish resale rights. Review commercial permission for each enabled deployment before public selling; internal availability is not `public_sell`. Customer BYOK platform fees remain subject to their existing fail-closed gate. |
| Operations and acceptance | Distinct serving/migration financial DB roles; accounting retention and migration readbacks; alerts for stuck holds, settlement lag, rating failures and reconciliation drift; concurrent/retry/expiry/late-settlement acceptance and actual invoice reconciliation. |

Money remains exact, never binary float. Technical usage and upstream provider
cost remain distinct from the customer's charge. Price versions cannot be
reused with changed rates, and missing prices never mean zero. Corrections use
compensating entries. An in-flight hold or ambiguous processor result must be
reconciled, not charged again.

## Rollout and retirement

1. Publish compatible identity contracts and deploy the verified identity
   boundary. Preserve the old economic endpoints for existing operations.
2. Complete Kaana admission, settlement and dispatch for an explicitly reviewed
   first-party population, with session classification and legacy-key fencing.
   Verify the actual artifact, policy, migration and a real request before
   calling that population live.
3. Complete commercial balances, limits, payments, seller/fiscal setup and
   invoices with the same durable authority and accounting invariants.
4. Approve a concrete funded-account migration from the read-only inventory;
   stop new old-authority admissions for that population, drain or explicitly
   transfer its holds, reconcile, record unique transfer entries and activate.
   Do not use unconstrained dual writes or duplicate spendable balances.
5. Retire old admission only after old operations and payment/outbox work are
   drained and historical reads remain available. Report transport, identity,
   internal metering, commercial accounting and invoice readiness separately.

The owner has already decided who owns economic policy. The outstanding seller
input is the legal entity/fiscal configuration; configuration and financial
writes wait for that concrete answer. This does not block identity or bounded
internal admission implementation. Production source state and release state
remain distinct under [the inference rules](../inference-rules.md#source-state-is-not-production-state).
