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

## Mutation boundary

`kaana-credential-control` is a separate long-running task. It exposes one
signed, action-discriminated route:

```text
POST /internal/v1/customer-provider-credentials/mutations
```

The exact request body is signed with `X-Oxy-Kaana-*` Ed25519 headers and the
dedicated `oxy-kaana-credential-control:v1` domain. The task trusts only
`KAANA_CREDENTIAL_CONTROL_PUBLIC_KEYS`; inference signatures cannot authorize a
mutation even if a key is accidentally repeated in both public-key sets. The
JSON is strict and versioned. Duplicate, unknown, cross-action and
trailing fields are refused. A secret is sent as strict base64 only inside the
signed TLS body, decoded to a clearable byte slice, encrypted immediately and
never logged or returned.

Actions are:

- `create`: exact identity plus secret; allocates a Kaana handle at revision 1;
  an existing identity returns `409` and its already-bound opaque reference but
  never rotates it;
- `rotate`: exact identity, handle, `expectedRevision` and secret; writes
  revision + 1 only when every selector matches one active row;
- `revoke`: exact identity, handle and `expectedRevision`; terminally revokes
  the row and advances the revision.

Rotate and revoke are optimistic-concurrency controlled. Replaying the same
signed mutation inside the signature window conflicts after the first success,
so a retry cannot rotate twice or revoke a later generation.

The control task uses the `kaana_customer_credential_control` PostgreSQL role.
It may execute only the three `SECURITY DEFINER` mutation functions and has no
table DML or select. Its KMS role gets `kms:Encrypt` on the configured key and
must not get `kms:Decrypt`.

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

The migration refuses unless `kaana_customer_credential_control` already exists.
Infrastructure must create that no-login role/login mapping and give its task a
separate verified-TLS `DATABASE_URL` before migration. Runtime never applies DDL.
