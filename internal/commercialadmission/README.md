# Commercial admission coordinator (not activated)

This optional package connects `nativeadmission` and `billing` in the **same PostgreSQL transaction**. Serving may opt in explicitly; the default constructor still has no commercial hooks. No default enrollment, account provisioning or funding source exists. It implements the accounting integration authorized by the owner for ADR 0010; it does not select a seller, issue an invoice or transfer money.

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

The actual serving connection selects the separately provisioned `kaana_commercial_runtime` NOLOGIN group, inheriting **only** `kaana_admission_runtime` and `kaana_billing_runtime`. The admission role already has EXECUTE on the exact provider-cost evidence function; borrowing the broader `kaana_runtime` role is unnecessary and would create a membership cycle with the existing serving login. It must never inherit billing admin or either migration role. Commercial links/outcomes are append-only; runtime cannot provision funds or alter account limits. The serving PostgreSQL tests use that exact group through a nonsuperuser login, with separate migration and admin connections for explicit synthetic test funding.

## Validation and remaining work

Run `make check`, then `make test-integration` with an owned `KAANA_POSTGRES_TEST_URL`. The coordinator PG tests cover reserve rollback, ownership, fences/idempotency, restart without hooks, concurrent completion replay, financial/native commit failure, malformed or substituted charge rollback, failed/cancelled/partial/zero/rating-error outcomes, unknown expiry, late evidence and lifetime rejection. They use no production policies or balances.

Before commercial activation: provision and verify the reviewed roles, schemas and serving configuration; configure seller/legal entity, merchant/payment integration, verified funding, currencies, tax/invoice governance and retention; establish reconciliation and alerts; and review the authority cutover. No payment, invoice, cash refund, account migration or commercial activation is performed by these packages. Usage compensation in `billing` remains distinct from external refunds and cannot convert promotional funds into cash.

## Explicit serving configuration and offline preparation

`KAANA_NATIVE_COMMERCIAL_ENABLED=true` selects the commercial runtime constructor;
only absence or `false` disables it, and other values fail startup. Enabling it
without the existing reviewed native policy path/hash pair fails. A policy with
`economicTreatment: commercial` is refused at startup unless this option is on.
The opt-in does not rewrite or activate any policy, account or legacy fence.
Internal policies still create no hold or debit, even in a mixed server.

Startup requires verified PostgreSQL TLS, the fixed composite role, installed
native/billing/coordinator schema and actual allowed/forbidden privileges. Neither
the composite group nor its login may have administrative memberships. Serving
can reserve/settle/expire through existing security-definer functions, but cannot
create or fund accounts, modify budgets, write journals directly, change native
policy/enrollment or create schema objects. An enabled billing account in the
principal owner's quoted currency, funded balance/explicit credit and active
budget remain required at admission. An attributed user never selects the payer.

Provisioning is a separate reviewed operator action, not performed by this code:

- Create `kaana_commercial_runtime` as NOLOGIN INHERIT, without elevated role
  attributes. Grant it `kaana_admission_runtime` and `kaana_billing_runtime` with
  INHERIT TRUE and SET FALSE. Grant the serving login SET membership in this group,
  INHERIT FALSE, without ADMIN OPTION. Do not grant admin or migration membership.
- Provision the existing billing NOLOGIN roles separately. Give only the migration
  login SET access to `kaana_billing_migrator`; it needs CREATE on the database and
  USAGE/CREATE on `public`, USAGE on `kaana_admission`, REFERENCES on
  `kaana_admission.admissions`, and SELECT on `public.kaana_admission_migrations`.
  These grants confer no native enrollment or policy-writing authority on the
  billing migration role. The billing admin is a separate trusted operator.
- Run native migrations first. Then execute **`kaana-native migrate-commercial`**
  with the dedicated migration connection. This fixed command takes no arguments,
  has a two-minute deadline, verifies every packaged native migration checksum,
  and runs billing followed by coordinator migrations under their exact role.
  Each package checks its own immutable checksum and rejects unknown versions.
  Replay is idempotent. The schemas commit separately: failure after billing
  commits may leave prepared billing schema, but never enrollment, accounts or
  funds; fix prerequisites and replay. Serving rejects an incomplete schema.
- No deploy workflow runs this command automatically. No current task definition
  gains this opt-in or new permissions in this change. Review an explicit future
  infrastructure rollout before enabling it, after seller/legal and commercial
  cutover prerequisites are resolved.

The existing native reconciliation loop invokes coordinator recovery when opted
in, with the same one-second schedule, bounded context and batch100. It replays
staged completion evidence, expires abandoned native executions, then releases
expired financial holds. Errors remain visible as pending reconciliation and are
retried without repeating provider inference. Disabling hooks during unresolved
commercial work refuses commercial replay; it never turns a debit into an internal
`not_charged` receipt. Preserve recovery until all financial outcomes reconcile.

This source wiring does not assert commercial activation, seller readiness,
payments, invoices or funds migration. Long commercial realtime sessions still
fail the existing14-minute execution bound; internal30-minute sessions are unchanged.
