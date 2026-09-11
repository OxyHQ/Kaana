# Provider key pools

Several providers, and a pool of keys for each: what a failure says about a KEY rather than about a request.

## Several providers, and a key pool for each

A provider slug in the inventory resolves to an adapter, an address and a pool
of credentials. None of those three is in the inventory: a credential there
would be a copy of an Oxy entity, and an address there would make one process's
reachability a global fact. The inventory names the slug; `cmd/kaana` resolves
it.

**One protocol serves several providers.** OpenAI, OpenRouter, Cerebras, Groq,
xAI, Mistral, DeepSeek, SambaNova, SiliconFlow and AI21 all expose an
OpenAI-compatible Chat Completions surface, so they are a `Config` and a base
URL rather than separate adapters. Provider-specific discovery remains
separate: compatible chat payloads do not imply compatible model catalogs.
The Messages API is refused under any slug but `anthropic`: that adapter reports
its slug as a constant, so serving it under another name would attribute every
event and every usage record to a provider the inventory did not route to.

**A provider owns a pool, but a deployment binds one exact key.** The pool is
the custody, reload and health container. Execution resolves the signed opaque
`deploymentId` through `provider_deployment_credential_bindings` to one exact
`(provider, keyId)` row. It never walks into a second platform key: Oxy exposes
that capacity as another deployment and orders it explicitly. A missing,
disabled or provider-mismatched binding fails before an upstream call.

### The distinction the design turns on

"This key has no capacity left" and "this key is momentarily throttled" are the
same HTTP status at every OpenAI-compatible provider, and opposite answers.
Treating them alike retires a healthy credential for fifteen minutes because a
provider asked for one fewer request this second — a worse failure than the one
a pool solves. So a failure is classified before anything reacts to it, and by
the code the ADAPTER chose from the provider's own error type, never by a
status:

| Verdict | What it means | What happens |
|---|---|---|
| `healthy` | the failure says nothing about the key — a timeout, a network failure, a provider 5xx, a throttle | the key stays; the request does not move |
| `exhausted` | the provider reported that this key's account has nothing left | the exact key is retired; a platform request cannot escape its deployment binding |
| `rejected` | the provider refused this credential: revoked, invalid, or lacking access | the exact key is retired; a platform request cannot escape its deployment binding |
| `request_fault` | the request is what was refused | nothing is retired and nothing is retried |

**An exhausted or refused key retires; a request fault does not.** The generic
pool walker remains covered for discovery and adapter conformance, but the
production executor supplies a one-key exact view, so another platform key can
only be reached through another Oxy-signed deployment. Authentication rejection
belongs to the credential sent, while a malformed request would fail identically
on every key.

**A request fault is retried nowhere.** The next credential would be refused
identically, so a rotation turns one customer error into several upstream calls
on a failure only the customer can fix.

**Unknown is not exhausted.** Every failure this build cannot classify —
a wrapped transport error, a code from a later contract, an upstream that named
no reason — is `healthy`, which is to say it says nothing about the key. That
default is the whole design: guessing exhaustion from an ambiguous signal
disables working credentials, and there is no signal that a *working* credential
emits to correct the guess.

**Nothing is retired forever.** A quota resets on the provider's own cycle and a
revoked key comes back when an operator rotates it, so a retirement is a flat
window (`KAANA_PROVIDER_<SLUG>_KEY_RETIREMENT`, default 15 minutes) rather than a
doubling backoff — a backoff models an outage of unknown length, which is what
the deployment breaker is for. A provider that said when its capacity returns is
believed over the window. The alternative, a key that never returns, is a
process that has retired every credential it holds, serves nothing, and cannot
be told otherwise without a restart; one failing round trip every quarter of an
hour is the price of never being in that state.

**A throttle rotates only where the operator said the keys are separate
accounts.** Keys of one account share that account's rate limit, so rotating
into it hammers a provider that has just asked for less traffic; keys of
separate accounts have separate limits and the next one can serve the request.
Kaana cannot tell which it holds, so it does not guess:
`KAANA_PROVIDER_<SLUG>_KEYS_ON_SEPARATE_ACCOUNTS=true` states it. Even then a
throttle rotates at most once per request, because nothing is retired on one and
an unbounded walk would repeat in full on every request for as long as the
throttle lasted.

### How a key becomes known-exhausted

In preference order, and the order is the point — an explicit report beats an
inference every time:

1. **The provider's own refusal**, classified by the adapter from the provider's
   error TYPE. `insufficient_quota` on an OpenAI-compatible provider,
   `billing_error` on Anthropic. This is the strongest signal there is: the
   provider, about the exact credential that was sent, and it is what this build
   runs on.
2. **A response header, through a mapping that provider DECLARED.** The mapping
   lives in the adapter package beside the code that speaks the wire format, not
   in operator configuration, and it maps a header to a MEANING rather than
   reading a name for one. `x-ratelimit-remaining` looks like a quota signal at
   every provider and is a burst limit at most of them, so a build that assumed
   generic names would retire a healthy key every time it was throttled.

   **The shipped mapping is empty**, with the count asserted exactly rather than
   left to be noticed: no provider served here documents a remaining-credits
   header on a completion response, and no live provider call has been made from
   this repository to verify one. A plausible guess is exactly the failure the
   mapping prevents. An entry arrives with a verified source to name and a count
   to move.
3. **Operator configuration**, which needs no mechanism: the key list is the
   configuration, and an operator who knows a key is spent removes it.
4. **Unknown**, the default, which disables nothing.

Not implemented, and named rather than stubbed: **an official quota API or an
authenticated usage endpoint**, which would sit above the header mapping. Every
provider spells one differently, none of them can be exercised from a repository
with no credentials and no live call, and a poller written against documentation
alone would be the same guess in a more expensive form.

### What a key pool is not

**It is not a route.** The pool stores and observes keys; its exact bound view
contains only the key named by the deployment. Reaching another platform key is
a route change to another signed deployment and emits `route_switch`, including
when both deployments use the same provider slug.

Choosing among deployments is permitted only by `authorizedRoutes`. Kaana
never uses provider, pool position or class to escape an exact binding.

**A route switch can only happen before anything has been streamed.** Once a
body is being read the request is committed to the exact deployment/key that
opened it; a mid-stream failure never moves to another credential.

**A request makes at most as many upstream calls as the pool has keys**, because
a key is never leased twice for one request. Nobody configured that ceiling; it
is a property of the walk.

**No credential enters anything.** Not a `Call`, an error, a log, a usage record
or the health projection. A key's identity outside `internal/provider` is its
`Key.ID` — the immutable opaque database id, never derived from the secret
(`internal/provider/credential.go`, `Key`). It is not a truncated hash, because
a fingerprint of a secret confirms a guessed secret. `Position` was the only
identity before `KeyID` existed and survives as the operator's declared order,
not as identity. `GET /internal/v1/health` reports, per provider, how many
credentials are declared, how many are usable, and which are out and until when;
a provider answering a probe with part of its pool spent reports `degraded`
rather than `ok`, because a pool draining towards empty is otherwise invisible
until the request that finds it empty.



[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0006-oxy-kaana-boundary.md

## Key class, and what "free first" is and is not

A key carries a `KeyClass` the operator STATES — `free`, `paid`, or unstated.
It is never inferred, and that is a measurement rather than caution: on
2026-08-23 no provider this build serves published remaining credit. Groq and
xAI publish burst limits whose counts refill; OpenRouter publishes no such
header at all, and its account endpoint answered `total_credits: 0` while a
completion on the same key really was billed. Nothing observable separates a
free-tier key from a funded one.

Class remains protected operator metadata and every live/cost observation is
attributed to the exact key. It does not select a key during platform execution:
Oxy orders compatible free, discounted, promotional and paid deployments, then
Kaana executes each exact deployment/key binding. Unstated remains distinct
from paid so telemetry never invents an economic fact.

`Key.ID` is the immutable opaque identity of one database row and is what a log
or health projection should use when an operator must act on that exact row.
It is never a provider/product name and never comes from the secret. `Position`
is only the explicit pool order copied with the row; it is not identity and no
admin operation resolves a credential through it.

## Where the credentials come from

PostgreSQL is the only durable store. Each key is one row carrying its provider,
opaque id, pool position, declared class, optional budget metadata and KMS
ciphertext. The task environment carries no provider secret, and no manifest or
inventory can carry one.

KMS authenticates `provider + keyId` as encryption context. A database write
that swaps ciphertext between rows therefore produces a decryption failure,
not a credential silently serving under another identity. The configured KMS
key ARN is also checked against every row before decrypting it.

The serving and publisher roles can select only the active-row view and decrypt
with that one KMS key; disabled historical ciphertext is not visible to their
database role. They cannot insert, update, migrate or encrypt. A short-lived operator
task uses `kaana-credentials put`, which accepts plaintext only on stdin,
encrypts before PostgreSQL sees it and never returns it. Adding a key is a row;
rotating one is an atomic upsert of the same `(provider, keyId)`.

Pools are loaded at process start in `position` order. A declared provider with
no active row is a startup refusal: reporting a green adapter that cannot
authenticate is no longer a supported state. Serving and publisher then reload
the complete database/KMS pool atomically on
`KAANA_CREDENTIAL_RELOAD_INTERVAL` (default `1m`). A failed reload keeps the
previous complete generation, so no request observes a pool assembled across
two credential revisions. A restart is required for provider-set or adapter
configuration changes, not for an ordinary database credential rotation.

Retirement state is durable by exact `(provider, keyId)`. Every real upstream
attempt records only its opaque request, deployment and key identities, verdict
and observation time; no secret or request content enters that record. When a
retirement expires, PostgreSQL grants one short recovery lease across all Kaana
replicas. Only the real inference request holding that lease may prove recovery;
health and catalogue probes never consume it. A successful request writes a
`usable` watermark, so an older failure arriving late cannot resurrect a stale
retirement; another exhaustion or rejection renews it. Rotating the ciphertext
under the same key ID clears the old generation's state atomically.

## Rules a reviewer applies

A credential is a POOL per provider, and the pool is a different rotation from
the deployment breaker. `internal/provider/credential.go` holds all of it. The
sections above carry the reasoning; these are the lines a reviewer holds a
change to.

### Custody of platform keys

- **PostgreSQL is the only durable provider-key store.** A provider key never
  enters environment, a GitHub secret, argv, a manifest, an inventory or a
  tracked file. The one-time `import-ssm` command may read a legacy SecureString
  directly through the AWS SDK; delete that source after verification.
  `DATABASE_URL` is a database credential, not a provider key.
- **PostgreSQL stores KMS ciphertext only.** KMS encryption context binds every
  ciphertext to `provider + keyId`; moving the bytes to another row must make
  decryption fail. The serving task gets `kms:Decrypt`, never `kms:Encrypt`.
- **Administration accepts plaintext only on stdin or directly from the legacy
  SSM API.** `kaana-credentials put` never accepts a value flag or environment
  variable; `import-ssm` never emits the fetched value. List operations do not
  initialize KMS or select ciphertext.
- **A key's durable identity is its exact opaque PostgreSQL key ID.** Pool
  position is only explicit spending order, never an admin or discovery
  selector. Neither is the secret or a hash of it, since a fingerprint confirms
  a guess.
- **Class is stated, never inferred**, and **unstated is not paid** — the
  measurement behind both is in "Key class" above. An unclassified pool keeps
  the order it was declared in, so classifying one key moves that key and
  disturbs no other.
- **A 402 is the platform's account refusing to be billed**, and it must retire
  the key. It reached the default branch once and became `invalid_request`,
  which told the customer their request was at fault and kept spending an
  account that cannot pay. The contract code is `provider_billing_refused`
  (`architecture.md`, finding 6).

### Rotation

- **A key leaves rotation only when something REPORTED that it has nothing
  left** — the provider refusing with its own exhaustion error, or a header that
  provider's declared mapping says means remaining credits, reading zero.
  `unknown` is not `exhausted`, `unavailable` is not `exhausted`, and every
  failure nobody classified leaves the key exactly as it was.
- **An exhausted or REFUSED key is retired.** Production does not walk from its
  exact deployment binding to another key.
- **A request fault is retried on nothing.** The next credential would be
  refused identically.
- **The verdict is read from the code the ADAPTER chose, never from a status.**
  `CredentialVerdictFor` is the one function, as `AttributableCategory` is for
  the deployment; the two answer different questions and disagree on purpose.
- **Another key requires another signed deployment and a route switch.** A
  shared provider slug does not imply a shared credential identity.
- **A rotation happens only before the response body is read**, so a failure
  arriving mid-stream rotates nothing: the request is committed to the key that
  opened the stream.
- **A retirement is a flat window, never permanent and never a backoff.** The
  provider's own reset time wins over the window.
- **A quota header mapping is per provider, lives in the ADAPTER package, and
  maps a header to a MEANING** — never a generic name. The shipped mapping is
  empty under an exact-count assertion; an entry needs a verified source.

### Providers, protocols and configuration

- **A provider slug resolves to an adapter, an address and a pool in
  `cmd/kaana`, never in the inventory** — a credential there is a copy of an Oxy
  entity, an address there makes one process's reachability global.
- **Provider slugs are not a closed list; PROTOCOLS are.** A build can only
  construct an adapter it contains, so an unknown protocol is refused; a slug
  that declares a protocol and a base URL needs no Go change.
- **Provider configuration and provider secrets have separate authorities.**
  Protocol, base URL, headers and pool policy are non-secret task environment;
  keys are ordered rows in PostgreSQL and are decrypted only after the adapter
  configuration is validated. Two slugs folding onto one configuration prefix
  are refused, never resolved.
- **The snapshot, the adapter set and the credential list move on different
  clocks, and no pairing may be fatal.** An undeclared provider in a snapshot is
  a WARNING, not a refusal to start: stopping takes every supported provider
  down over one unsupported one, and only on the next restart. A credential
  delivered for a provider nobody serves is warned about here because nothing
  outside the process can see it.
