# Customer provider credentials

Every upstream provider key has one durable home: KMS ciphertext in Kaana's
PostgreSQL database. That includes a customer's BYOK key. Oxy owns the
connection's metadata, validation, scope, eligibility and policy; it stores only
the opaque `credentialHandle` Kaana returned and the current revision beside
that metadata. It never stores plaintext or a Vault, SSM, Secrets Manager or
other independently resolvable secret locator.

## Exact identity

One customer ciphertext is bound to all of these values:

```text
provider + ownerAccountId + connectionId + environment
+ credentialHandle + revision
```

The first four are immutable opaque values supplied in a signed Oxy mutation.
Kaana does not parse them or turn them into local account/connection records.
`credentialHandle` is 128 random bits allocated by Kaana and returned as a
lowercase `kcred_…` value. `revision` starts at one and advances on every rotate
or revoke.

Every value above is authenticated by the KMS encryption context. Copying a
ciphertext to another provider, owner, connection, environment, handle or
revision makes decryption fail. The PostgreSQL identity is unique by exact
`ownerAccountId + connectionId + environment`; it is never selected by provider
display name, insertion order or a partial match.

Every control-plane write also carries an Oxy-minted opaque `operationId`. The
id is globally unique in Kaana's operation ledger and is bound to the exact
action, provider, owner, connection, environment, requested handle and expected
revision. It is never derived from any name or database order. The wire value
is case-sensitive, 1–128 characters, and restricted to `[A-Za-z0-9_-]`.

## Mutation boundary

`kaana-credential-control` is a separate long-running task. It exposes one
signed mutation route and one signed exact-outcome route:

```text
POST /internal/v1/customer-provider-credentials/mutations
POST /internal/v1/customer-provider-credentials/outcomes
```

The exact request body is signed with `X-Oxy-Kaana-*` Ed25519 headers and the
dedicated `oxy-kaana-credential-control:v1` domain. The task trusts only
`KAANA_CREDENTIAL_CONTROL_PUBLIC_KEYS`; inference signatures cannot authorize a
mutation even if a key is accidentally repeated in both public-key sets. The
JSON is strict and versioned. Duplicate, unknown, cross-action and trailing
fields are refused, and each signed body is capped at 16 KiB. A secret is sent
as strict base64 only inside the signed TLS body, decoded to a clearable byte
slice, encrypted immediately and never logged or returned.

Mutation actions are:

- `create`: `operationId`, exact identity and secret; allocates a Kaana handle
  at revision 1. A different operation for an existing identity returns a
  reference-free `409`; it never adopts or rotates the existing secret;
- `rotate`: exact identity, handle, `expectedRevision` and secret; writes
  revision + 1 only when every selector matches one active row;
- `revoke`: exact identity, handle and `expectedRevision`; terminally revokes
  the row and advances the revision.

All three writes reserve `operationId` and finish its outcome in one PostgreSQL
transaction. An applied mutation changes the credential and appends its audit
row in that same transaction; a failed compare records a durable conflict
without changing or auditing the credential. Create and rotate also bind a
fixed 32-byte SHA-256 fingerprint computed from the decoded secret. The
fingerprint is never returned or logged; it exists only so reusing one operation
id with a different secret fails closed. Replaying the same id with the same
complete request returns the first terminal outcome without writing again.
Reusing it with another action, actor, identity, handle, revision or secret
returns a reference-free conflict. Two different operation ids racing on one
expected revision produce exactly one `applied` result and one durable
`conflict`.

The signed outcomes route carries no secret. Its strict body repeats
`schemaVersion`, `operationId`, `action`, the four identity fields and, for
create/rotate, the lowercase 64-character `secretSha256` already recorded with
the pending Oxy row. Rotate/revoke also repeat the exact `credentialHandle` and
`expectedRevision`. An exact match returns:

```json
{
  "schemaVersion": 1,
  "operationId": "<opaque Oxy operation id>",
  "action": "rotate",
  "status": "applied",
  "credentialHandle": "kcred_<26 lowercase base32 characters>",
  "revision": 2
}
```

`status` is `applied` or `conflict`; a conflict omits handle and revision. An
absent operation and any mismatched query field are deliberately the same
`404 outcome_not_found`. This is the only reconciliation read: there is still
no credential metadata, ciphertext or plaintext read route.

The control task uses the `kaana_customer_credential_control` PostgreSQL role.
It may execute only the three `SECURITY DEFINER` mutation functions and the one
exact outcome function, and has no table DML or select. Its KMS role gets
`kms:Encrypt` on the configured key and must not get `kms:Decrypt`.

## Inference-only resolution

The serving task has the inverse authority: the `kaana_runtime` PostgreSQL role
may execute one exact active-row function and its KMS role gets `kms:Decrypt`,
never `kms:Encrypt`. `CustomerResolver.ResolveForInference` requires the handle,
revision and all four identity values, returns the same generic unavailable
result for absence/revocation/mismatch/decryption failure, and is the only API
that yields plaintext. There is deliberately no HTTP get/list/resolve route.

The published inference envelope does not yet carry a Kaana credential handle.
Therefore this change does not enable BYOK execution by guessing from provider,
account or connection metadata. The rollout gate is a new `@oxyhq/contracts`
field on each exact authorized route, followed by Oxy signing that binding and
Kaana wiring only that field into `ResolveForInference`. Until both halves ship,
BYOK routes remain ungrantable and the resolver is unreachable from inference.

## PostgreSQL

Migration `0003_customer_provider_credentials.sql` creates:

- `customer_provider_credentials`, containing ciphertext and exact identity;
- `customer_provider_credential_audit`, with create/rotate/revoke identity and
  actor but no plaintext or ciphertext;
- a metadata view for credential administrators;
- three mutation functions for the control role and one exact active resolver
  for the runtime role.

Migration `0004_customer_credential_operation_outcomes.sql` replaces the three
pre-idempotency mutation functions with atomic operation-aware functions and
adds `customer_provider_credential_operations`. The table contains exact
non-secret request identity, terminal outcome metadata and the write-only
32-byte fingerprint needed to reject a changed-secret replay. It contains no
plaintext, ciphertext or resolvable secret locator. The control role has no
table select; it reconciles through the exact outcome function, which never
returns the fingerprint. Existing audit rows remain valid, while every new
audit row has a unique operation id.

The migration refuses unless `kaana_customer_credential_control` already exists.
Infrastructure must create that no-login role/login mapping and give its task a
separate verified-TLS `DATABASE_URL` before migration. Runtime never applies DDL.
