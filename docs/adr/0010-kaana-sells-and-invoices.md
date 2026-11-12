# ADR 0010 — Kaana sells and invoices; Oxy keeps identity

- Status: proposed. The owner decided the direction on 2026-11-12 ("Kaana sells
  and invoices; Oxy keeps accounts"). Nothing here is built. Until phase 1 ships
  and this ADR is accepted, **Oxy is still the ledger** and every document that
  says so is still true of production.
- Date: 2026-11-12
- Supersedes, once accepted: ADR 0001 decision 2 ("Oxy owns customers and
  limits... Oxy bills from Kaana's receipt"), ADR 0002 decisions 1 and 3 and the
  economic-authority consequence of its 2026-10-15 amendment, and the "Kaana
  never prices" rule in `AGENTS.md`, `inference-rules.md`, `architecture.md` and
  `README.md`.
- Oxy's half: Oxy ADRs 0005, 0006, 0009 and 0014 assign billing to Oxy and need
  a matching superseding decision in that repository. This ADR cannot change
  them.

## Context

Today the money path is split like this (read from the code on 2026-11-12):

- **Oxy** authenticates the caller, applies policy, **reserves** a hold before a
  request runs, **rates** units at the price version it copied from Kaana's
  declared catalogue, writes the **ledger**, refunds the difference, keeps
  **receipts**, collects payment and issues **invoices**. About 15,000 lines of
  services, about 30 tables, the Console billing UI. Payments go through
  **Peable**, Oxy's payments platform (hosted checkout, subscriptions, tax,
  refunds, disputes, webhooks): Oxy's plans and subscriptions have been on
  Peable since 2026-11-04 (Oxy ADR 0046). Account-scoped inference top-ups are
  still named after Stripe in Oxy's code (`stripeAccountBilling.service.ts`,
  `billing_stripe_events`); whether that path still reaches Stripe directly or
  already goes through Peable must be confirmed in the Oxy repository before
  phase 3. Stripe is otherwise only Peable's internal card provider, never a
  merchant-facing dependency.
- **Kaana** relays the caller's credential, receives a signed **grant**,
  resolves routes inside it, executes, and writes one **settlement** per grant
  to an outbox: units only, no amount (`internal/publicapi/settle.go`,
  `inference_settlement_outbox`). It publishes the declared list prices
  (`configs/public-catalogue.json`, 249 of 260 lines) and records its own
  upstream cost (`internal/providercost`, `provider_cost_events`).

The owner wants Kaana to charge and invoice its customers. Oxy stays the
identity provider: accounts, organizations, `oxy_sk_` keys and the Console are
not what is moving.

## Decision

1. **Kaana is the seller of record for inference.** It decides what a request
   costs a customer, holds and spends the customer's balance, charges it, and
   issues the receipt and the invoice.
2. **Oxy keeps identity.** An account, an application, a credential and a
   scope are still Oxy's. Kaana stores the Oxy account id as an opaque string,
   as it does today (`docs/architecture.md`: an Oxy id is never parsed or used as
   a primary key). A balance row is keyed by a Kaana-minted id that carries the
   Oxy account id as an attribute, never as the key.
3. **The grant stays, and changes meaning.** Oxy still answers "who is this and
   what policy applies" (principal, scopes, routing policy, model lines the
   caller may use, provider and region limits, funding restriction for products
   such as Alia Pro and Ultra). The **hold, the price and the ceiling move to
   Kaana**: Kaana sizes the hold from the declared price and the request's
   facts, reserves it in its own ledger before executing, and settles it after.
4. **Units stay the atom.** Settlement is still exact, per-attempt, partitioned
   units (`UsageReport`). Amounts are derived from units and an immutable price
   version, never stored as the only fact.
5. **First-party traffic is a policy, not a ledger bypass.** `internal_metered`
   relationships (Alia, Clarity, Mention, Astro) keep taking no hold and producing
   no receipt. Who owns their capacity and USD ceilings is an open question below.

## What the ledger must guarantee

These are carried over from the Oxy implementation because they are what makes
it trustworthy; the new one is not accepted without them:

- Append-only journal; corrections are compensating entries; money is exact
  `NUMERIC`, never a float.
- A reservation and its settlement-plus-refund are each one transaction, idempotent
  on `grant_id` / `settlement_id`. A different body for a known id is a conflict.
- Holds expire (15 minutes today) and a sweeper releases them.
- A charge above its hold is refused and the hold stays until it expires.
- A failed, cancelled or empty (zero-usage "completed") request is not charged.
  Kaana's own upstream cost row still keeps the real units, because the provider
  bills them regardless.
- Price versions are immutable. Reusing a `priceVersionId` with different prices
  is an error (today: `price_version_changed`, priced at nothing). A unit with no
  price is `unpriced_unit`, a served line with no price is `model_not_priced`;
  nothing settles at zero by absence.
- Draw order: promotional funds, then purchased credit, then invoiced.
- Payments are not the ledger: the ledger is the authority and is reconciled
  against Peable, which cannot refuse a request in real time.

## What has to be built or moved

| Capability | Today | Where it goes |
|---|---|---|
| Balance, holds, journal, receipts, refunds | Oxy (`inferenceLedger.service.ts`, ~2,200 lines; `billing_ledger_*`, `usage_*` tables) | Kaana database and a Kaana billing service |
| Rating (units x price version) | Oxy, at settlement, from a price copy taken at authorization | Kaana, at settlement, from the declared catalogue |
| Price versions and their receipt snapshots | Kaana assigns ids; Oxy snapshots them (`usage_receipt_unit_prices`) | Kaana |
| Spending limits and budgets | Oxy (`spending_limits`, per application) | decision needed: Kaana enforces, Oxy sets? |
| Payments, auto-recharge, subscriptions, credit grants | Oxy, through Peable (top-ups possibly still Stripe-named) | Kaana, through Peable |
| Invoices and the monthly close for invoiced accounts | Oxy (`accountInvoicing.service.ts`) | Kaana |
| Tax and VAT on inference invoices | **not built** for inference in Oxy; Peable carries a tax product | Kaana, using Peable's tax, once the answers below exist |
| Reconciliation against the processor | Oxy (`billingReconciliation.service.ts`) | Kaana |
| Receipts API (`GET /v1/generations`) | relayed from Oxy, amount included | served by Kaana |
| Console billing pages | Oxy Console | Oxy Console, reading Kaana's API |

## Preconditions found in the code

These make a straight port unsafe and have to be dealt with first:

- **Per-process state.** Two `kaana` tasks run in production. Breakers, latency
  and throughput maps, affinity, the per-credential rate limiter and BYOK
  throttles are per process. A balance cannot live that way: every balance
  decision must go through PostgreSQL, with row-level locking or an equivalent.
- **The contract has no customer-money shapes** beyond the relayed `Money`
  (`internal/contract/grant.go`). New shapes mean new `@kaana.ai/contracts`
  versions and the descriptor flow.
- **The database package is named for credentials.** `internal/credentialstore`
  also holds the settlement outbox, realtime owners and cost events. Billing gets
  its own package, schema and DB roles (the role-per-concern model already used
  for the credential tasks), and the billing service is separate from the serving
  hot path.
- **No retention policy.** `provider_cost_events`,
  `provider_credential_attempt_events`, `inference_settlement_outbox` and the
  audit tables grow without bound. Billing records need a stated retention.
- **No metrics.** There is no metrics endpoint. A ledger needs alerting on stuck
  holds, outbox lag, rating failures and reconciliation drift.
- **`KAANA_SETTLEMENT_SIGNING_KEY` is read from an environment variable** and is
  documented nowhere. That key signs settlements today; its custody must be
  settled before it signs anything financial.
- **Webhook ingress.** Peable's webhooks need an unauthenticated but signed
  route with its own signature domain, following the `edgeauth` pattern. Peable's
  own guide (`docs/integrating-peable.md` in that repository) and its SDK define
  the events and the signature.

## Phases

0. **Decide and publish** (this ADR plus the answers below, and Oxy's superseding
   decision). No behaviour changes.
1. **Shadow rating.** Kaana rates every settlement itself from the declared
   catalogue and compares with Oxy's receipt amount, writing differences to a
   table and raising an alert. Oxy stays authoritative. This proves price
   versions, units and rounding before any money moves.
2. **Kaana ledger for a new population.** New accounts, or one opted-in
   application, are held and charged by Kaana. Oxy's ledger keeps everyone else.
   The grant carries which authority applies.
3. **Payments and invoices** through Peable, with reconciliation.
4. **Migration** of the remaining balances and open reservations, with a dual-write
   window and a cutover date.
5. **Retire** Oxy's inference ledger and edge, and update the rules.

Each phase ships behind a flag and is reported as rolled out only after a real
request has completed with the expected receipt, per `inference-rules.md`.

## Open questions

The owner has to answer these before phase 1 can finish and phase 2 can start.
None is decided here.

1. **Seller of record.** Which legal entity bills, and in which currencies?
2. **Peable.** Which Peable merchant workspace (Orbit) does Kaana bill under, and
   does Kaana call Peable directly with its own credentials or through Oxy? Are
   inference top-ups in Oxy already on Peable or still on Stripe?
3. **Tax and VAT.** Peable has a tax product, but nothing for inference invoices
   exists in Oxy. Registration, tax-inclusive or exclusive prices, invoice
   numbering, and who is responsible.
4. **Margin and discounts.** Oxy's ADR 0002 amendment says Oxy keeps "descuentos y
   margenes sobre el precio de lista", yet no such code was found in the grant and
   settlement paths. Is the list price the customer price, and where do overrides
   live?
5. **Free tier and promotional credit.** Who owns allowances, promotional funds
   and the `promotional-only` hold restriction?
6. **Alia Pro and Ultra.** Their credits-only ceiling lives in Oxy's policy and the
   grant's `maxFundingTier`. Does it stay a grant field Oxy sets?
7. **Internal traffic.** Do Alia, Clarity, Mention and Astro stay unbilled and
   unreceipted, and who holds their capacity ceilings?
8. **BYOK platform fee.** Who sets the schedule? The approved list is empty and
   production is fail-closed.
9. **Refunds, disputes, chargebacks and credit notes.** Policy and who handles
   them. Promotional funds are not refundable today.
10. **History.** What happens to open reservations, receipts and invoices already
    in Oxy's ledger, and what is the cutover date?
11. **Receipts API.** Does `GET /v1/generations` keep the shape Oxy's receipts
    have today?
12. **Spending limits.** Who sets and who enforces per-application budgets?

## Consequences

- Kaana stops being a pure data plane. The rules that say so (`AGENTS.md`,
  `architecture.md`, `inference-rules.md`, `README.md`, ADR 0001 and 0002) are
  rewritten when this is accepted, not before, because they describe production.
- A second money authority exists during phases 2 to 4. The shadow phase and the
  per-account authority field exist to keep it from being two ledgers for one
  customer.
- Oxy's `kaana_grants` schema, its settlement endpoint and the grant contract all
  change with it. The change is coordinated, reader first, as ADR 0004 did for
  first-party authority.
