# Inference rules across Oxy

The rules every Oxy repository follows when it touches model inference. Kaana's
own mechanisms are in the documents linked from each rule; the developer guide
for building an AI feature is [Oxy's inference README](https://github.com/OxyHQ/oxy/blob/main/docs/inference/README.md)
(exact model, power level, app default, who owns what, request examples, errors).

```text
one-shot feature   app -> Kaana (https://kaana.ai/v1, Oxy grants it) -> provider
conversation/agent app -> Alia -> Kaana -> provider
first-party service Oxy edge -> Kaana (signed /internal/v1 lane) -> provider
```

The two ways in, and who decides what on each, are in [lanes.md](lanes.md).

This is the accepted successor boundary (ADR 0010), not a claim that every
caller or financial population has migrated. Existing Oxy economic grants
remain the transitional authority until verified cutover.

Which product feature takes which path is kept in one place,
[Oxy's request-routing.md](https://github.com/OxyHQ/oxy/blob/main/docs/inference/request-routing.md).

## Rules

- **Kaana's name and origin are exact.** Kaana (this Go repository) is the
  inference data plane. Its only canonical signed origin is `https://kaana.ai`;
  never introduce a Kaana hostname under `oxy.so`. Code, AWS configuration and
  signed headers use `kaana`, `KAANA_*` and `X-Oxy-Kaana-*`. Historical
  identifiers may remain only as allow-listed migration inputs until retired
  ([identity-and-routing.md](identity-and-routing.md#one-name-and-one-origin)).
- **Apps and Alia never call a provider.** No provider adapter, key pool, route
  executor, retry loop, failover list or model substitution in an app or in
  Alia. Kaana retries the same route on transient failures and fails over along
  the routes Oxy authorized (a signed list, or a grant Kaana resolves), before the
  first output ([routing.md](routing.md)). A second copy of that logic in a caller
  is a bug.
- **Say "power level" to users; "routing profile" is the technical term.** An
  exact model is `publisher/model[@revision]` and is never substituted. A power
  level is a slug with no `/` (`auto`, `instant`, `medium`, `high`, `xhigh`,
  `pro`, `ultra`). Alia shows levels, never model names.
- **Kaana is not the agent runtime.** Conversations, tools, memory, approvals
  and agents belong to Gemies (including Alia) ([identity-and-routing.md](identity-and-routing.md#kaana-is-not-alia)).
- **Kaana owns inference policy and spending; Oxy supplies identity.** The
  owner accepted this boundary in [ADR 0010](adr/0010-kaana-sells-and-invoices.md).
  A verified identity alone grants no inference or spending permission. Kaana
  owns model eligibility, funding restrictions, capacity, prices, holds and
  settlement for its enrolled population. Preserve reviewed terms through the
  migration; product names and public client IDs never select an unbilled policy.
- **Each admission keeps its authority.** Legacy Oxy grants retain their
  signed routes and settle in Oxy. Kaana-native admissions persist their own
  policy and price snapshot before execution. A retry or timeout cannot switch
  ledgers; existing idempotency bindings must be fenced before cutover.
- **Technical usage, provider cost and customer charge are different facts.**
  Measured units are not an amount. Provider cost stays private to operator
  accounting. Customer amounts belong only in the canonical financial/receipt
  contract, rated at immutable prices; identity assertions contain none.
- **Provider credentials live only in Kaana's encrypted PostgreSQL store**
  (KMS ciphertext). Never in product databases, environment variables, task
  definitions or bundles. An Oxy application key identifies a product (Alia,
  Mention, Homiio); it is not an upstream provider key
  ([key-pools.md](key-pools.md)).
- **A deployment executes on its provider's key pool, or on one pinned key.**
  Kaana owns every provider key and walks the pool cheapest stated funding first
  (free, credits, paid, unstated) within the admission's funding ceiling; an
  operator pin to an exact `(provider, keyId)` is final and never falls back to
  the pool ([key-pools](key-pools.md),
  [ADR 0001](adr/0001-kaana-owns-provider-keys-costs-and-routing.md)).
  The production cutover to this runtime is recorded in
  [schema-0013-cutover-2026-09-24.md](schema-0013-cutover-2026-09-24.md).
- **Kaana is PostgreSQL-only.** MongoDB is neither a fallback nor a migration
  target.
- **`itsfree.ai` is a discovery lead only.** It has no licence. Copy none of its
  source, prose or data; re-derive every claim from the provider's own
  documentation or authenticated API.

## Source state is not production state

A merged change proves the contract and its gates, not that a task, migration,
credential rekey or canary has run. A cutover is complete only when the runbook
has read back the running artifact and configuration, and a real request has
completed with the expected Oxy attribution and Kaana usage receipt. Until then,
call it a candidate or a pending rollout. A merge to `main` is source evidence;
**live** requires production readback and acceptance. Report accepted design,
implemented source and verified production separately. The request v2 candidate validation is
[Oxy's kaana-request-v2-cutover runbook](https://github.com/OxyHQ/oxy/blob/main/docs/runbooks/kaana-request-v2-cutover.md).
