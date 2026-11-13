# Commercial ledger foundation

This package implements the transactional accounting core of [ADR 0010](../../docs/adr/0010-kaana-sells-and-invoices.md). The optional [commercial admission coordinator](../commercialadmission/README.md) joins its transactions to native admission and settlement. Neither package is connected to the serving process, a payment processor or invoices. No currency, account, credit or enrollment is seeded. Merely constructing `Store` does nothing to the database. The existing production economic authority is unchanged by this package.

## Identity and roles

Kaana mints currency, account, hold, journal, receipt and refund IDs. The Oxy owner is an opaque attribute of the account; `(owner, currency)` locates its Kaana identity, but the owner is never the primary key. Currency and owner attribution cannot be rewritten. All operations use the Kaana account ID; an eventual authenticated API must resolve that ID from trusted identity, never accept arbitrary caller-selected account ownership.

Run `Store.Migrate` explicitly with a dedicated migration connection. Provision these **NOLOGIN** roles separately, then grant narrowly scoped membership to workload identities:

- `kaana_billing_migrator`: owns the schema and immutable migration checksum ledger; needs CREATE on the database and USAGE/CREATE on `public` to install it. The inference process never receives this role.
- `kaana_billing_admin`: can explicitly provision an account, configure its budget/credit ceiling, record verified funding, and compensate a consumed charge.
- `kaana_billing_runtime`: can reserve, settle and expire, and read the billing records. It cannot create accounts, change budgets, mint credit, compensate charges or directly edit any table.

Database mutation is confined to fixed-search-path `SECURITY DEFINER` functions. Their private helpers are not executable by either operational role. PostgreSQL permissions and immutable-record triggers are exercised with actual role connections in the integration tests, including positive authorized controls.

## Monetary lifecycle

Amounts cross the Go boundary as exact decimal strings and are stored as `NUMERIC(38,12)`. Values with exponents, negative amounts or more than twelve decimal places are rejected, not rounded. Pricing belongs to the existing native admission/rating layer. This package consumes its validated amount, price snapshot ID/digest and immutable usage receipt ID/digest; it does not invent a tariff, decide an unpriced unit is free or accept a floating-point price.

`CreateAccount` starts disabled with zero funds and zero credit. A finite budget window and explicit `ConfigureAccount` activation are required before new holds. A zero credit limit is prepaid; a positive limit is an explicitly authorized invoiced ceiling. Reconfiguring the current budget never moves old holds or consumption to a different window. A refund reduces the original window's consumption. Window/credit policy changes require the admin role and are not public endpoints.

`Reserve` locks the account row, checks the active window's held plus spent amount, and draws promotional funds, purchased credit, then invoice credit. The resulting hold records the exact split, originating budget and immutable price identity. Its deadline is database time plus a caller-requested **1–900 whole seconds**. Invoice outstanding includes both held and consumed invoice credit, so concurrent requests cannot overdraw it. An operation replay returns the original hold with `Replay=true` and its current state; **only a fresh hold authorizes a new execution**.

`Settle` locks the same account. It inserts the immutable receipt, consumes the held split and releases every unused portion in one transaction. A charge above the hold is refused without changing it. Completed or partial usage may consume a validated rated amount; failed/cancelled requests and completed requests with no usage charge zero. The caller must supply actual usage evidence even for a free result. Missing or failed rating must never be represented as a successful zero-rated input. A zero-charge receipt is still durable evidence of completion, not a missing record.

A timely settlement retry returns its original receipt even after the deadline. A different settlement key for a settled hold is a conflict. A report arriving after an unsettled hold's deadline atomically expires/releases it and returns `State=expired`, with **no receipt and no new debit**. It must not be presented as a charged completion. `Expire` also sweeps a bounded batch (1–1000 holds), atomically releasing each. A coordinator must schedule this sweeper before any account is activated; no timer is installed by this library.

`Refund` is an approved **usage compensation**, unwinding invoice credit first, purchased funds second and promotional funds last. Cumulative compensation cannot exceed the receipt. It appends records instead of rewriting history. It is not a cash refund or processor request: promotional credit never turns into purchased funds or cash. External payment refunds, chargebacks and invoice credit notes are separate workflows not implemented here.

## Evidence and accounting invariants

All money-changing decisions serialize on the account row. Holds, receipts, funding and compensation have scoped operation keys and canonical fingerprints including account/currency/owner attribution and the relevant evidence. Reusing a key with different content conflicts. Global admission, verified funding evidence and usage evidence uniqueness also prevent a new operation key or account from consuming the same external fact twice. Price snapshot IDs cannot silently acquire a different digest.

The append-only journal uses transfer rows: each row encodes `debit(source)` and `credit(destination)` of the **same exact amount**. The `postings` view exposes both signed legs. Balance is structural, not a later repair; split funding/holds produce several balanced transfers. Table privileges prevent operational roles from creating arbitrary postings or editing balance projections. Immutable triggers additionally reject rewriting journal, receipt, refund, funding, price and reservation identity records.

No prompts, generated text, provider credentials or user IPs are accepted. Evidence references are bounded opaque IDs/digests, not request payloads. Records are retained without automatic deletion in this foundation; financial retention, access/export and legally approved deletion policies must be established before commercial activation. It does not assert an invoice retention period or seller identity.

## Validation and operational prerequisites

The PostgreSQL suite creates a random disposable database per test and uses dedicated migrator/admin/runtime roles. Set `KAANA_POSTGRES_TEST_URL` to an owned test instance and run:

```sh
go test -race -count=1 ./internal/billing/...
```

It covers concurrent reserve/settle/compensate, stable retries and changed-body conflicts, account/currency isolation, budget/credit caps, precision, rollback after a receipt has been inserted, cancellation while waiting for a lock, bounded expiry, late reports, zero-charge outcomes, price identity and role permissions. `make test-integration` includes this package and the coordinator; a skipped suite is not a PostgreSQL validation.

Before commercial traffic can use this library, the following work remains:

1. Bind trusted authenticated accounts to an explicit authority/cutover policy, with legacy-key fencing and no double billing. Keep first-party metered enrollment distinct from commercial balances.
2. Wire the tested coordinator into the serving constructor and recovery scheduler. Its transactional hooks already bind native planning/rating, reserve-before-execution and durable settlement retries, with authenticated native receipt reads. Operational configuration and activation remain absent.
3. Configure the seller/legal entity, supported currencies, merchant workspace, pricing/discount governance, tax treatment, invoice numbering and applicable retention. None is selected here.
4. Integrate verified payment/grant events and reconciliation through the approved payment system. Implement cash refunds, disputes, invoice collection and credit notes separately; this package does not send money or issue invoices.
5. Add operational metrics/alerts for stuck holds, failed rating/settlement, evidence conflicts and reconciliation drift, then run reviewed cutover and reconciliation checks.

Until these are complete and reviewed, the package remains a tested library with no runtime caller and no financial activation.
