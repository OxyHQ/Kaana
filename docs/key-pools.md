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

**A deployment is (provider, upstream model); it executes on its provider's
key pool.** Kaana owns every provider key and chooses among them
([ADR 0001](adr/0001-kaana-owns-provider-keys-costs-and-routing.md)). Execution
resolves the signed opaque `deploymentId` by the first rule that applies
(`provider.Registry.ResolveExecution`):

1. **An operator pin** — an exact row in
   `provider_deployment_credential_bindings`. It is final: the deployment uses
   only that key, and a pin to a retired or disabled key, or one naming another
   provider, never falls back to the pool. Pins are optional; they exist for a
   key that must serve one deployment alone (a contract, a test, a scoped
   permit) and are removed with `kaana-credentials unpin-deployment`.
2. **The provider's pool.** Every enabled key of the provider, cheapest stated
   funding first — free, then credits (a promotional or trial grant before a
   prepaid balance), then paid, then unstated — and within one funding the
   declared position. A request walks it: a key that is spent (`exhausted`),
   refused (`rejected`) or, for the same request's retry, failing transiently
   before any output is passed over for the next key. A request fault never
   moves.

A provider with no enabled key is a startup refusal, so rule 2 always has a key
to start from. Every attempt, cost and audit record names the exact key that
served, whichever rule chose it.

**A funding ceiling bounds the walk.** When Oxy authorizes a route only on
cheaper capacity (Alia Pro and Ultra spend credits only, Oxy ADR 0038), the
signed route carries `maxFundingTier` (contract set 3.8.0) and Kaana walks
only keys at or below that tier (`KeyPool.WithinFunding`); an unstated key is
never admitted under a ceiling. A realtime session's routes obey the same
ceiling. A route with no admissible
key is skipped like an unroutable one, and a route whose admissible keys are
all retired fails over along the signed routes with the earliest return time —
it never falls through to a dearer key.

A retired key returns through the same recovery lease as before: once its
retirement expires, the first real request that reaches it in pool order (the
cheapest key first) holds the cross-replica lease, and its success writes the
`usable` watermark. Nothing else probes it.

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
| `exhausted` | the provider reported that this key's account has nothing left | the exact key is retired and the request moves to the next key of the pool (a pinned deployment has none) |
| `rejected` | the provider refused this credential: revoked, invalid, or lacking access | the exact key is retired and the request moves to the next key of the pool (a pinned deployment has none) |
| `request_fault` | the request is what was refused | nothing is retired and nothing is retried |

**An exhausted or refused key retires; a request fault does not.** The walk
moves the same request to the next key before anything is delivered, so the
customer never learns a key was spent. Authentication rejection belongs to the
credential sent, while a malformed request would fail identically on every key.

**A transient failure retires nothing but is remembered for the request.** A
throttle, an overload, a timeout, a provider 5xx or a transport failure before
any output says nothing durable about the key, so the key stays in rotation.
The executor's same-route retry (`routing.md`) then starts on a different key
of the pool while one is usable — the request's `CredentialAttemptSequence`
records which keys failed it transiently — and falls back to the same key only
when no other can serve. A new request starts on the cheapest key again.

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
authenticated usage endpoint**, which would sit above the header mapping. The
inventory publisher does not probe one either (`inventory.md`, "Withheld from
publication", says which providers were checked); an operator who reads a
balance records it as capacity evidence, which publication reads. Every
provider spells one differently, none of them can be exercised from a repository
with no credentials and no live call, and a poller written against documentation
alone would be the same guess in a more expensive form.

### What a key pool is not

**It is not a route.** Moving to another key keeps the deployment: same
provider, same upstream model, another credential. It emits no `route_switch`,
because nothing about the customer's route changed. Choosing among DEPLOYMENTS
is still permitted only by `authorizedRoutes`; a pin is never escaped for the
pool, and a funding ceiling is never escaped for a dearer key.

**A key change can only happen before anything has been delivered.** Once a
body is being read, that ATTEMPT is committed to the exact key that opened it;
a mid-stream failure never moves to another credential. What the executor may
still do, while no output has reached the customer, is start a new attempt
(`routing.md`, "Same-route retry"), which is a fresh walk that prefers keys this
request has not seen fail. A single-key pool reuses its key, which is correct
because a throttle is not exhaustion. Where an operator declared
`KeyPolicy.OnSeparateAccounts`, a walk may also spend its one throttle rotation
before any body is read; the executor's attempt cap bounds how many walks one
request can make.

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

## Funding, and what "free first" is and is not

A key's **funding** orders the pool and is what a funding ceiling admits
(`internal/provider/funding.go`). It is derived from two STATED facts, never
inferred: the key row's class (`free`, `paid` or unstated) and its protected
description's capacity category (`trial`, `promotional`, `prepaid`, `paid`).
When both are stated the dearer reading wins. That is a measurement rather than
caution: on 2026-08-23 no provider this build serves published remaining
credit. Groq and xAI publish burst limits whose counts refill; OpenRouter
publishes no such header at all, and its account endpoint answered
`total_credits: 0` while a completion on the same key really was billed.
Nothing observable separates a free-tier key from a funded one.

| Funding | From | Tier | Order |
|---|---|---|---|
| `free` | class `free` | free | 1 |
| `promotional` | category `promotional` or `trial` | credit | 2 |
| `prepaid` | category `prepaid` | credit | 3 |
| `paid` | category `paid`, or class `paid` | paid | 4 |
| unstated | nothing stated | none | 5 |

The order is the owner's: free, then credits, then paid, for the same model.
Within the credit tier a grant is spent before a prepaid balance (the grant
costs nothing and expires). An unstated key is spent last and never under a
ceiling, because the worst case of what it costs is paid; it is still not
REPORTED as paid, so telemetry never invents an economic fact.

Every live and cost observation is attributed to the exact key, and Oxy's
operator feed carries each attempt's key and that key's economics (`cost.md`).
The usage report Oxy settles from names the funding tier of the key that
served (`fundingTier`: absent when unstated, customer-owned or nothing was
served). Neither carries a key id or label to a customer. Neither the key id nor its
label ever reaches a customer.

`Key.ID` is the immutable opaque identity of one database row and is what a log
or health projection should use when an operator must act on that exact row.
It is never a provider/product name and never comes from the secret. `Position`
is the explicit pool order within one funding; it is not identity and no admin
operation resolves a credential through it.

## Protected credential metadata

A key's identity is its opaque `(provider, keyId)` and nothing else. What an
operator knows about it lives beside the secret row, never in it (migration
`0017`), and only `kaana_credential_admin` can read or write it:

- **Funding account** — an opaque `kfa_` id and a protected label such as the
  account's email. Keys naming the same account are the operator stating they
  share capacity; Kaana never infers two keys are independent. An account's
  label never changes under its id.
- **Description** — title, capacity category (`trial`, `promotional`,
  `prepaid`, `paid`), environment, commercial-use eligibility with the
  evidence it rests on **for that exact key** (never inferred from the
  category: a trial key can carry an explicit grant), and optional
  model/capability restrictions. Every put is kept whole under its `kcm_`
  operation id; a replay is idempotent and a reused id with another document
  fails closed.
- **Capacity evidence** — append-only quota, balance or expiry observations,
  each with its source and `freshUntil`. Evidence without `freshUntil` is never
  fresh, so a stale balance is never read as a current one.

`kaana-credentials put-credential-metadata` and `record-capacity-evidence` read
their JSON document from stdin (never argv, where an email would sit in shell
history); `list-credential-metadata` shows descriptions with the keys that share
each funding account and the latest evidence of each kind. The label-free part
is what Oxy reads on the operator feed (`cost.md`).

## What publication reads from a key

The inventory publisher withholds a discovered deployment while NO key it may
execute on can serve it (`inventory.md`, "Withheld from publication"): its pin
alone, or every enabled key of its provider. It judges each key by the
following evidence, read without decrypting anything or taking any lease, and
publishes as soon as one key is clear:

- the key's runtime state: retired until `retired_until` withholds until then;
  an expired retirement does not, because the first real request after it is
  the only thing that can prove recovery;
- the key's latest fresh capacity evidence (`0017`): zero balance, zero
  day/month/lifetime quota, or a passed expiry withholds until it stops being
  fresh; zero over a minute or an hour is a throttle and does not;
- the deployment's attempts on that key since its last success (`0019`): one
  billing or credential refusal withholds for a quarantine, unless capacity
  evidence reading more than zero was recorded after it.

A deployment every key withholds is withheld until the earliest moment one of
them may return.

So an exhausted key comes back on its own: its retirement expires, the
quarantine after its last refusal ends, the deployment is published for a
trial, and the first real success writes the `usable` watermark and ends the
streak. Rotating the ciphertext clears the runtime state and discards the
older attempts as evidence (they describe the previous secret). Recording
fresh positive capacity evidence ends a refusal's quarantine early; it does not
shorten a retirement, which serving enforces on its own clock.

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

An attempt's durable identity is `(request, deployment, index)`, and the index
is request-scoped across every walk the request makes (`routing.md`,
"Same-route retry"): the same identity with any other fact is refused by the
database. The record is written after the upstream has answered, so it is
bookkeeping, never the customer's answer: a write that fails is logged
(`credential attempt not recorded`, with request, deployment, index, key and
outcome) and the walk proceeds exactly as the verdict says. The in-memory pool
has already applied that verdict; what a lost record costs is cross-replica
knowledge, which the next real attempt on the key re-establishes.

The durable record is written from the exact view a deployment executes on, so
the view carries its pool's provider and policy. `kaana_record_provider_credential_attempt`
refuses an `exhausted` or `rejected` attempt whose `retired_until` is not after
its `occurred_at`. Until 2026-09-24 `KeyPool.Bind` built the view without them:
every bound 402, 401 or 403 recorded a zero-length retirement, the database
refused it, and the customer got a retryable `provider_error` instead of
`provider_billing_refused` or `provider_credential_invalid`, while the key
stayed in rotation.

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
- **Funding is stated, never inferred**, and **unstated is not reported as
  paid** — the measurement behind both is in "Funding" above. A pool is walked
  free, promotional, prepaid, paid, unstated, then by position; an
  unclassified pool keeps the order it was declared in.
- **A 402 is the platform's account refusing to be billed**, and it must retire
  the key. It reached the default branch once and became `invalid_request`,
  which told the customer their request was at fault and kept spending an
  account that cannot pay. The contract code is `provider_billing_refused`
  (`architecture.md`, finding 6).

- **Operator metadata is admin-only and never an identity.** A label, email or
  commercial-use evidence text is readable by `kaana_credential_admin` alone;
  no runtime path, feed or response carries it.

### Rotation

- **A key leaves rotation only when something REPORTED that it has nothing
  left** — the provider refusing with its own exhaustion error, or a header that
  provider's declared mapping says means remaining credits, reading zero.
  `unknown` is not `exhausted`, `unavailable` is not `exhausted`, and every
  failure nobody classified leaves the key exactly as it was.
- **An exhausted or REFUSED key is retired and the request moves to the next
  key of the pool** — never past a pin, never past a funding ceiling.
- **An exact view is the pool narrowed to one key, not a pool without a
  policy.** `Bind` copies the provider, retirement window, separate-accounts
  declaration and quota signals; a walk reads them from the view it was given.
  `TestABoundViewRetiresWithItsPoolsPolicy` and its PostgreSQL twin hold this.
- **A deployment with no pin executes on its provider's whole pool**, cheapest
  stated funding first. A pin is never escaped for the pool.
- **A funding ceiling admits only keys whose stated tier is at or below it**;
  an unstated key is never admitted under one, and a spent grant never falls
  through to a paid key.
- **A transient failure retires nothing; the same request's retry prefers
  another key** and reuses the failed one only when no other can serve.
- **A request fault is retried on nothing.** The next credential would be
  refused identically.
- **The verdict is read from the code the ADAPTER chose, never from a status.**
  `CredentialVerdictFor` is the one function, as `AttributableCategory` is for
  the deployment; the two answer different questions and disagree on purpose.
- **Another key is not another route.** Moving keys keeps the deployment and
  emits no `route_switch`; moving deployments needs the next signed route.
- **A rotation happens only before the response body is read**, so a failure
  arriving mid-stream rotates nothing: the request is committed to the key that
  opened the stream.
- **A retirement is a flat window, never permanent and never a backoff.** The
  provider's own reset time wins over the window.
- **Publication reads key state; it never writes it.** The publisher takes no
  recovery lease and records no attempt: a key proves recovery only through a
  real request, and a withheld deployment is re-published for exactly that.
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
