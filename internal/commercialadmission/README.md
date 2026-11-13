# Commercial admission coordinator (not activated)

This optional package connects `nativeadmission` and `billing` in the **same PostgreSQL transaction**. It has no serving-process caller, timer, default enrollment, account provisioning or funding source. It implements the accounting integration authorized by the owner for ADR 0010; it does not select a seller, issue an invoice or transfer money.

## Immutable authority and transactions

Every native Policy and Plan explicitly declares `economicTreatment`: `internal_metered` or `commercial`. Missing values are invalid; identity, brand, account existence or caller attribution cannot choose the treatment. The field participates in the immutable policy digest. Previously prepared enrollment documents must be reviewed and regenerated with the explicit value, not silently interpreted.

`nativeadmission.NewStoreWithEconomics(pool, coordinator)` fixes the hooks for the store's lifetime. `NewStore` continues to serve explicitly internal policies, but refuses commercial admission and commercial settlement replay. Restarting without the coordinator therefore fails closed. Internal metering and long internal sessions do not acquire a monetary hold.

Admission first enforces the native authenticated principal, credential, immutable plan, legacy-key fence, idempotency and capacity limits. Within that same transaction, the commercial hook resolves the authenticated owner's Kaana account in the quoted currency, reserves the full quote, and records the immutable policy and price snapshot digests. Delegated user attribution never replaces the authenticated owner. Funding failure rolls back the native admission, daily counter and financial changes together; provider execution cannot start from an unsuccessful admission.

Completion staging remains durable content-free usage evidence. Settlement uses native rating against the admitted price snapshot, then consumes the hold and stores both the financial and native receipt in one transaction. The native boundary validates the existing canonical `charge` schema, owner/application/credential attribution, served model, usage, outcome, exact rated amount and price snapshot before committing. A hook error, invalid receipt or later native write failure rolls back the debit too. Repeated evidence returns the same committed settlement; changed evidence conflicts. Native generation reads preserve their existing ownership checks and never expose a successful `metered_usage/not_charged` result for a debit.

Completed/partial positive rated usage is charged. Authoritative failed/cancelled outcomes and completed zero usage get durable zero-charge receipts and release the hold. Missing rating, an unknown outcome or a charge above the admitted quote is **not a free completion**. Failed rating is recorded as `rating_failed`, with no generation receipt; expiry releases its outstanding hold. Evidence arriving after an unsettled hold expired records `hold_expired`, with no new debit. These outcomes require operational reconciliation; the library does not infer usage or automatically retry inference.

## Lifetime and recovery

Billing holds last at most 15 minutes. The coordinator reserves the admitted execution duration rounded up to a whole second plus a one-minute settlement grace. Consequently commercial execution durations greater than **14 minutes** are rejected before hold creation or provider execution. Native internal realtime sessions may still last 30 minutes; commercial sessions of that length require a separately reviewed renewable-reservation design and are not enabled here.

`Recover` processes durable completions first, marks expired native executions abandoned, then expires outstanding financial holds. It attempts all three phases even if an earlier phase fails and reports committed counts plus errors. Unknown abandoned requests retain idempotency and never synthesize zero usage. A successful replay after an earlier crash settles once; replay after an expired financial hold remains explicitly uncharged/expired. A serving integration must schedule bounded recovery before enrolling commercial traffic.

## Database roles and migration

Run native and billing migrations first, then explicitly run `Coordinator.Migrate` using `kaana_billing_migrator`. In addition to billing migration grants, this role needs `USAGE ON SCHEMA kaana_admission` and `REFERENCES ON kaana_admission.admissions`. The coordinator creates its own `kaana_commercial` schema and checksummed migration ledger. It changes no native migration or production enrollment.

The actual serving connection must combine **only runtime grants**: `kaana_admission_runtime`, `kaana_runtime` (provider-cost evidence), and `kaana_billing_runtime`. It must never inherit billing admin or either migration role. Commercial links/outcomes are append-only; runtime cannot provision funds or alter account limits. The PostgreSQL tests use precisely this combined role, with a separate migration connection and a separate admin connection for explicit synthetic test funding.

## Validation and remaining work

Run `make check`, then `make test-integration` with an owned `KAANA_POSTGRES_TEST_URL`. The coordinator PG tests cover reserve rollback, ownership, fences/idempotency, restart without hooks, concurrent completion replay, financial/native commit failure, malformed or substituted charge rollback, failed/cancelled/partial/zero/rating-error outcomes, unknown expiry, late evidence and lifetime rejection. They use no production policies or balances.

Before commercial activation: connect the coordinator to the reviewed serving and recovery constructors; configure seller/legal entity, merchant/payment integration, verified funding, currencies, tax/invoice governance and retention; establish reconciliation and alerts; and review the authority cutover. No payment, invoice, cash refund, account migration or commercial activation is performed by these packages. Usage compensation in `billing` remains distinct from external refunds and cannot convert promotional funds into cash.
