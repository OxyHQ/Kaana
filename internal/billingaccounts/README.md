# Reviewed billing account operator

`kaana-native billing-accounts` closes the account-provisioning gap between the
commercial ledger and the separately operated Peable customer binding. It is a
private operator, not a public API, inference request, timer or startup action.
It exposes no funding, refunds, balance migration, enrollment, provider calls or
Peable writes. No configuration, role or production operation is shipped.

## Review and execution

Use a private config file containing exactly `enabled: true`, `ownerAccountId`
(the independently reviewed authenticated payer owner) and `currency` (explicit
three-letter code). Pass its independently reviewed byte SHA256; the command does
not derive approval from its own hash. There are no default owners, currencies,
amounts, credit limits, budget windows or activation flags. User attribution never
selects the payer. This config is not proof of authenticated identity: the trusted
operator must ground the owner and currency in the reviewed enrollment evidence.

```
kaana-native billing-accounts --config-file <private-reviewed-config> \
  --config-sha256 <reviewed-byte-hash> --operation-file <private-operation>
```

The operation file (or stdin when omitted) is bounded to64KiB and strictly decoded:
duplicate/case-variant/unknown/null fields and unused fields are rejected before
opening PostgreSQL. The process has a two-minute deadline. Database error text and
connection credentials are not emitted. Private output contains only the operation
receipt or scoped account snapshot, never customer profiles or payment payloads.

Operations:

- `{"action":"migrate"}` explicitly installs this package's checksum-ledgered
  migration after verifying the packaged billing migration checksum. It does not
  install billing/native/Peable migrations, create roles or seed accounts.
- `{"action":"provision","key":"<durable-key>","evidenceId":"<review-reference>"}`
  creates an empty disabled account in the configured owner/currency scope, or
  acknowledges an already empty disabled account. A new operation refuses an
  existing configured, enabled or funded account. Exact replay returns its original
  durable receipt even if the account was subsequently configured; it never resets
  the account. Use inspect for current state.
- `{"action":"inspect","accountId":"<account>"}` returns that exact scoped
  account's state and revision. Inspection takes a short account row lock for a
  consistent review; it writes no persistent data, but is not a READ ONLY transaction.
- Configure requires **all** fields: `action:"configure"`, `accountId`, `key`,
  `evidenceId`, `revision` from inspect, `budgetKey`, `startsAt`, `endsAt`, `amount`,
  `creditLimit`, and boolean `enabled`. UTC timestamps must have exactly milliseconds
  (`YYYY-MM-DDTHH:mm:ss.sssZ`). Money is an exact decimal string validated by the
  existing ledger amount function, not a second financial implementation. A zero
  credit limit does not fund prepaid usage; a positive credit limit explicitly
  authorizes invoice debt and must be reviewed as such. `enabled:false` is explicit.

Configuration locks the account (the same serialization boundary as reserve,
settle and compensation), compares the expected state, then calls the existing
budget/account functions and records the operation receipt in one transaction.
The revision covers credit/enabled state, all account balances and the active
budget's terms/held/spent quantities. Concurrent changes refuse stale review before
any mutation. Existing holds keep their original budget/window; reconfiguration
never transfers or forgives their usage. Exact operation-key replay acknowledges
its immutable original receipt without reapplying old settings. A key reused with
different terms, evidence or reviewed config conflicts. A lost output is recovered
by replaying the **same** operation, then inspecting current state; never replace a
key to evade an ambiguous result. Receipt persistence failure rolls back all edits.

## Authority and rollout

Provision `kaana_billing_account_admin` as a NOLOGIN group with no memberships or
administrative attributes. Its dedicated LOGIN has only SET membership in that
group, INHERIT FALSE, ADMIN FALSE. The group can execute only this package's three
scoped wrappers. It cannot call the broader billing admin's Fund/Refund/configure
functions, directly mutate financial tables, change native enrollment, or alter
schemas. Startup verifies both immutable migration checksum ledgers through
metadata-only SELECT grants; it never installs or repairs schema. Startup also
checks effective current and original session privileges, including
extra grants and ADMIN OPTION; RESET ROLE cannot bypass the checks.

Migration uses a separate dedicated LOGIN with SET-only membership in the existing
`kaana_billing_migrator`, which must own the already installed billing schema and
have database CREATE/public CREATE as documented by that ledger. Migration creates
only `kaana_billing_accounts` and its separate immutable checksum ledger; existing
billing, native and Peable migration bytes remain unchanged. No runtime receives
migration authority. The CLI never provisions or grants its own roles.

Before real operation, independently review the population, payer/currency,
commercial deployment rights, budget/credit terms, actual role grants and database
migration readback. Account setup alone neither enrolls native commercial policy
nor enables the serving constructor, verifies funding, binds a Peable customer,
issues invoices or reconciles payment. Those separate gates remain in the
[commercial runtime](../commercialadmission/README.md) and
[Peable operator](../peablebilling/README.md) runbooks. Reuse the already configured
Peable platform and legal entity; this operator does not select or configure a seller.
