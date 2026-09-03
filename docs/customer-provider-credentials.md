# Customer provider credentials

Every upstream provider key has one durable home: KMS ciphertext in Kaana's
PostgreSQL database. That includes a customer's BYOK key. The implemented Kaana
boundary mints an opaque `credentialHandle`, versions it and records durable
mutation outcomes without exposing a plaintext or ciphertext read route.

Source support is not by itself a production-availability claim. The
coordinated Oxy cut stores only the returned handle and revision beside
Oxy-owned connection metadata. Its authenticated edge source signs the exact
`ready + active + valid` resolver result and carries a separate platform-fee
version through reservation and settlement; Kaana can consume only that opaque
binding. BYOK stays ungrantable until the fee amount/version is approved,
published and associated, Oxy migration `0069` and matching releases are
deployed, the accounting, IAM/KMS, SSM, immutable-image and live-request gates
pass together, and a dedicated authenticated initial-validation bootstrap is
implemented. Neither side resolves an old locator or guesses by provider.

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
slice, encrypted immediately and never logged or returned. The decoded value
must be 1–4096 visible ASCII bytes, the exact common subset carried unchanged
by the providers' `Authorization` and `x-api-key` headers. Control bytes,
whitespace and non-ASCII values fail before encryption; the same validation is
repeated after decryption so an older or corrupt row cannot become a local HTTP
transport failure attributed to a shared deployment.

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
`schemaVersion`, `operationId`, `action` and the four identity fields.
Rotate/revoke also repeat the exact `credentialHandle` and
`expectedRevision`. Oxy never stores or sends a secret hash: when recovery has
to replay create/rotate, it resends the secret to the mutation route under the
same operation id and Kaana compares its internal digest there. An exact
metadata match returns:

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

The published inference envelope's optional
`authorizedRoutes[].customerProviderCredential` carries the exact handle,
revision, owner, connection and environment. After the envelope signature,
structural checks, inventory match and pure provider translation pass, the
executor gives precisely those values plus the route's provider to
`ResolveForInference`. An explicit JSON `null` is invalid rather than equivalent
to an absent binding. The executor builds a request-scoped one-key pool, clears
the resolver's byte slice and never substitutes the adapter's platform pool.

An absent, revoked, mismatched, stale or undecryptable generation terminates as
the generic non-retryable `byok_credential_invalid` before any upstream call.
An upstream authentication or billing refusal for that customer-owned key maps
to the same user-visible inference code, does not trip the shared deployment
breaker and does not fail over to another authorized route. Only the explicit
authentication refusal emits an `invalid/unauthorized` credential verdict;
billing refusal does not disable the generation and the separate bootstrap
classifies it `inconclusive/forbidden`, so funding the provider account permits
an explicit revalidation without rotating the key. A customer-account throttle preserves
`rate_limited` and the provider's retry hint but likewise cannot trip that
breaker or fail over. BYOK routes neither read nor write the platform
deployment breaker: a broken platform key cannot block a healthy customer key,
and a BYOK success cannot rehabilitate the platform credential lane. Other
deployment-attributable BYOK failures can still fail over inside that request's
exact signed route list, without retaining cross-customer breaker state.
Provider usage remains on the technical record, but it is explicitly marked
customer-billed and excluded from Kaana's provider-cost totals. Platform
credentials retain their existing pool, rotation, failover and cost behaviour.

This Kaana path does not grant a BYOK route. Oxy owns connection readiness,
scope and policy. Its edge source now places an exact active binding on each
selected BYOK route and settles against a separate platform-fee pointer, but
production still needs the fee amount/version to be approved, published and
associated, migration `0069`, matching releases and the IAM/network/SSM/live
gates. Until those pass, BYOK routes remain ungrantable even though Kaana can
execute a correctly signed binding.

The lifecycle is also Oxy-owned. Normal serving may bind only a
`ready + active + valid` generation. A `pending_validation + unvalidated`
generation never appears in normal `authorizedRoutes`; the binding has no
bootstrap-purpose flag.

## Separately authenticated bootstrap validation

Oxy explicitly selects one customer-safe catalogue deployment ID and durably
binds it to the protected exact Kaana deployment ID. It then signs a v1 task to
`POST /internal/v1/customer-provider-credentials/validations` under the
domain-separated `oxy-kaana-credential-validation:v1` signature. The task
repeats application, provider, owner, environment, connection, credential
handle/revision and deployment. Kaana never resolves any selector by name,
ordering or a first match.

Migration `0006_customer_credential_validations.sql` stores that exact operation
and a renewable execution lease whose monotonic generation must match at
completion. Reusing an operation ID with any selector changed returns conflict.
A live lease returns pending; an expired lease is reclaimed after a worker
restart, and its former worker can no longer commit. A terminal result is
replayed without another provider call and its service-authenticated Oxy
callback is emitted again. A
crash after an upstream answer but before the database commit can cause one
minimal at-least-once retry after lease expiry, but can never mark a credential
valid without a successful provider call.

The runner decrypts only the exact generation through the existing runtime
resolver, creates a request-scoped customer key pool, sends fixed text `.` with
`maxOutputTokens = 1` to only the exact deployment and discards all output. It
applies the configured probe deadline before PostgreSQL/KMS resolution and
carries that context through the upstream call. It
does not use the normal executor, user response stream, authorized-route
failover, shared breaker, usage report, receipt, or Oxy billing flow. The
upstream provider may still bill the customer's provider account for that tiny
call.

Classification is deliberately asymmetric:

- successful real call: `valid`;
- explicit provider authentication rejection: `invalid/unauthorized`;
- provider credit or billing refusal: `inconclusive/forbidden`;
- quota or throttle: `inconclusive/rate_limited`;
- timeout/network cancellation: `inconclusive/network`;
- missing exact route/generation: `inconclusive/not_found`;
- all other failures: `inconclusive/unknown`.

Only `invalid/unauthorized` disables a generation. Billing and quota never
pretend the key is cryptographically wrong. After top-up or quota recovery, Oxy
starts a new exact operation against the same handle/revision; no rotation is
required. The callback carries the complete exact task plus terminal state
through Kaana's narrow Oxy service principal, and Oxy atomically deduplicates
the operation and connection transition. A lost callback is recovered by
reposting the pending Oxy operation, causing Kaana to replay its terminal result.

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

Migration `0005_customer_credential_outcome_without_digest.sql` removes the
secret digest from the signed reconciliation query. The digest remains inside
the operation table and the atomic create/rotate functions, where it protects
mutation idempotency, but the exact outcome function accepts and returns only
non-secret metadata.

Migration `0006_customer_credential_validations.sql` adds only the bootstrap
operation ledger and its runtime `SECURITY DEFINER` claim/complete functions.
The runtime has no direct DML on the table; the credential-control role has no
validation execution authority.

The migration refuses unless `kaana_customer_credential_control` already exists.
Infrastructure must create that no-login role/login mapping and give its task a
separate verified-TLS `DATABASE_URL` before migration. Runtime never applies DDL.
