# Pensara

Pensara is the inference **data plane** for the Oxy platform. It normalizes a
request, translates it for one upstream provider, streams the result back,
propagates cancellation, and reports what was technically consumed.

It is not a product. It has no customers, no accounts, no console and no
billing. Oxy is the single control plane for all of that
([ADR 0005][adr0005], [ADR 0006][adr0006]); Pensara executes what Oxy has already
authorized and reports what it measured.

Tracked as workstream 13 of [OxyHQ/oxy#972][epic].

## It was called Relay

`Relay` was the working name. Everything a person reads now says Pensara: the
module path, the packages, the binaries' source directories, the environment
variables and the docs.

Two things are still answered under both spellings, because they are exchanged
with a service that deploys separately and there is no ordering of two deploys
that avoids a gap:

- **Environment variables.** Every variable is `PENSARA_*`; a deployment still
  setting `RELAY_*` is answered from that instead, current name winning. One
  function, `providerconfig.EnvName`.
- **The Oxy edge's signing headers.** `X-Oxy-Pensara-Key-Id`, `-Timestamp` and
  `-Signature` are preferred and the `X-Oxy-Relay-*` spellings still verify. A
  clean cut would refuse every request with `unknown key id` — before a
  signature is even computed — for as long as the edge kept sending the old
  names.

Both are migrations with an end, not compatibility layers. Deleting either turns
a named test red, which is how the deletion proves it did something:
`TestTheLegacySpellingIsAnswered` and `TestTheLegacyHeaderSpellingStillVerifies`.

Three things deliberately still say `relay` and are **not** oversights:

- The signed domain separator `oxy-relay-envelope:v1`, which lives inside the
  signature. Changing it changes every signature and is invisible to everyone.
- The shipped binary paths (`/usr/local/bin/relay`) and the inventory mount
  point (`/etc/relay`), which the task definition in oxy-infra names. They move
  with the infrastructure rename, in the same change as terraform.
- The AWS resource names and the deploy workflow's `APP`/`FAMILY`, for the same
  reason.
- The image's own `ENV` defaults. A container definition's `environment` beats
  an image `ENV`, but the binary prefers `PENSARA_*` — so an image setting the
  new spelling while the task definition still sets the old one would make the
  IMAGE default win, inverting the override. Both hold the same value today, so
  the swap would break nothing and teach nothing. They move with terraform.

---

## The boundary, in one paragraph

Pensara owns request normalization, provider adapters, routing **execution**,
streaming, cancellation, model deployments, provider health and technical
metering. Pensara never owns accounts, organizations, projects, members,
applications, credentials, balances, a billing ledger or a customer console. It
stores Oxy identifiers as **immutable, opaque strings** and never as records it
may create, edit or delete. Authorization, attribution, scope checks and spend
reservation all happen **in Oxy, before a request reaches Pensara** — Pensara does
not re-derive them, and an envelope that does not carry them is refused.

If a change would put an Oxy-owned concept in this repository, the change is
wrong, not the boundary. `AGENTS.md` states the rules a reviewer applies.

## Layout

```
cmd/pensara/                      the serving binary: env config, wiring, graceful drain
cmd/pensara-publisher/            the publishing binary: builds the inventory and re-issues it
internal/awssig/                AWS SigV4, so the publisher can write one S3 object
internal/contract/              Go types for @oxyhq/contracts' inference module
  descriptor.json               GENERATED from the published package
  descriptor_test.go            the drift gate
  drift_test.go                 the gate's positive control
  fixtures_test.go              wire fixtures for the Zod round-trip
internal/edgeauth/              Ed25519 verification of the Oxy edge's signature
internal/httpapi/               the Oxy-facing HTTP surface
internal/inventory/             deployment inventory and its configuration snapshot store
internal/provider/              the Adapter interface, registry and error vocabulary
  credential.go                 per-provider key pools: what a failure says about a KEY
  conformance/                  the suite every adapter must pass
  openaicompat/                 the ported OpenAI Chat Completions adapter
  anthropic/                    the ported Anthropic Messages API adapter
internal/providerconfig/        where a provider lives: the defaults table both commands read
internal/providercost/          what a request cost Pensara upstream; never a customer amount
internal/publisher/             builds the inventory from the providers' own model lists
internal/pensara/                 the executor: routing, failover, framing, usage reports
internal/rotation/              per-deployment circuit breakers and health scoring
internal/sse/                   SSE decoding (upstream) and encoding (downstream)
tools/contract/                 Node tooling that derives and checks the contract
configs/inventory.example.json  an illustrative inventory snapshot
configs/inventory.json          the measured Cerebras + OpenRouter snapshot, for a publisher to re-issue
configs/model-attribution.json  who RELEASED the weights a provider serves; the publisher's only editorial input
configs/provider-rates.example.json  illustrative upstream rate cards
```

Layers depend downward only: `httpapi → relay → {inventory, rotation,
providercost, provider} → contract`, with `sse` as a leaf. `contract` imports
nothing of Pensara's, which is what lets the drift gate compare it against the
published package with nothing in between — and specifically it cannot reach
`providercost`, which is asserted rather than reviewed
(`TestTheContractCannotReachAnAmount`).

**Go** is the implementation language, as the epic prefers for a
high-concurrency streaming data plane. Nothing here argued against it: a
per-request goroutine with a cancellable `context.Context` threaded to the
upstream HTTP call *is* the cancellation design, rather than something layered
on top of it.

## The contract is not re-invented here

`@oxyhq/contracts@0.29.0` (contract version 1.1.0) is the wire contract, and the Go types in
`internal/contract` are hand-written against it. Hand-writing is only safe
because two independent gates fail when the two sides diverge.

**1. A generated descriptor, compared field by field.**
`tools/contract/generate.mjs` imports every module under the published package's
`inference/` directory and emits `internal/contract/descriptor.json`: every
shape, field, optionality, kind, enum member, literal value and string
constraint. The shape list is **discovered, not listed**, so a shape added
upstream appears on the next regeneration.
`internal/contract/descriptor_test.go` then requires that:

- every published shape is either implemented in Go or recorded in an exact,
  reason-carrying `notApplicable` list (an exact count, so a shape cannot be
  excused by appending a line);
- every implemented shape's Go JSON field set **equals** the contract's — no
  extra field, no missing field;
- optionality matches (optional ⇔ absentable and `omitempty`, required ⇔ neither);
- enum members match exactly, in both directions;
- the regexes Pensara enforces are character-identical to the published ones;
- `ContractVersion` equals the published `INFERENCE_CONTRACT_VERSION`.

CI regenerates the descriptor and fails on any diff, which catches a hand-edit
and a version bump nobody re-derived. The bump to `0.28.0` is what that gate is
for: it brought 25 shapes from two new modules — account billing, entitlements
and reconciliation — every one of which is a control-plane concept this
repository is forbidden to hold, so all 25 are recorded not-applicable with the
reason naming the owner, and `expectedNotApplicableCount` moved in the same
change.

**What it catches:** a field renamed, added, removed, or flipped between
required and optional; a scalar's type changed; a reference repointed; a version
literal changed; a variant added to a discriminated union; an enum member added
or removed. `drift_test.go` proves this rather than asserting it — it perturbs
the descriptor in each of those eleven ways and requires the comparator to
report every one, after first confirming the unperturbed comparison is clean.

**2. Pensara's own output, parsed by the real Zod schemas.**
Structure is not values. `go test ./internal/contract/...` marshals one fixture
per wire shape using the same Go types the server uses — with **every optional
field populated**, because an optional field that drifted is invisible in a
minimal fixture — and `tools/contract/validate.mjs` parses each with the
published schema itself. It also feeds six deliberately invalid fixtures that
the schemas must **reject**, and fails if it saw no fixtures at all: a validator
with a broken schema lookup would otherwise report the same clean run.

Regenerate after a version bump:

```bash
cd tools/contract && npm ci && npm run generate
```

## The Oxy-facing surface

```
POST /internal/v1/inference    signed envelope in, normalized event stream out
GET  /internal/v1/health       signed; the customer-safe provider projection
GET  /livez                    unsigned liveness; no provider or route detail
```

An HTTP status answers exactly one question: was this a well-formed envelope
from the Oxy edge? Once the answer is yes the response is `200` and an event
stream, and every outcome after that — including a refusal — arrives as the
stream's terminal `error` event. The alternative cannot be made to work: a
request that fails after two hundred tokens has already sent `200`.

Frames are named. `event: stream_event` carries a contract stream event;
`event: usage_report` carries the technical usage record. The framing is
Pensara's own — the contract specifies shapes and says nothing about transport.

**Envelope versioning is a hard gate.** `schemaVersion` is read before anything
else is interpreted; an unrecognised version is refused whole. A version is
never inferred from the presence of a field. Conversely, **unknown fields are
tolerated**: the contract states that adding an optional field is additive, so a
strict decoder would turn every additive Oxy change into an outage here.

**Edge authentication is Ed25519 over the exact body** — Pensara holds only public
keys, so it cannot construct an envelope it would itself accept. This is a
decision Oxy has not made; the reasoning, and why it is not an HMAC, is in
`internal/edgeauth`. It is deliberately one small file to replace.

## Cancellation

A client disconnect cancels the upstream provider call. The proof is split in
two, and each half is mutation-tested — the mutation was applied, the test was
observed to fail, and the file was restored:

| Link | Test | Mutation that must break it |
|---|---|---|
| client → executor | `internal/httpapi`: `TestClientDisconnectCancelsExecution` | `Execute(context.Background(), …)` instead of `r.Context()` |
| adapter → upstream | `internal/provider/conformance`: "a client disconnect cancels the upstream call" | `http.NewRequestWithContext(context.Background(), …)` in the adapter |

Both compare against a **control run that is not cancelled**, because "the
upstream saw its caller go away" is also what a request that simply finished
reports. The cancelled run must show the upstream observing the disconnect
*before* it wrote every chunk; the control must show it never observing one and
writing all of them.

A cancelled request still produces a usage report with the units measured up to
the cut, and settles as `cancelled`. A partial stream is a settlement case, so
an adapter that returned nothing on cancellation would make an exact refund
impossible.

## Same-model failover

When a deployment fails in a way another one could survive, the same **model
revision** is retried somewhere else. Never a different model: the platform
forbids serving weights the customer did not ask for, and a fallback that
crossed models would look exactly like a success.

**That distinction is structural, not a rule someone remembers.** A reference
resolves to an `inventory.RouteSet`: one model reference, and the endpoints that
serve it. The reference is stored **once, for the whole set**, and an
`inventory.Endpoint` carries none of its own — so a candidate that names
different weights is not a case that has to be excluded, it is a value that
cannot be constructed. `RouteSet.Candidates()` is the only place a route is
built from an inventory, and it stamps the set's single reference onto every
one. Two guards sit on top of that shape, and both are mutation-tested:
`TestAnEndpointCannotCarryItsOwnModelReference` fails if a future change gives
an endpoint its own model, and the emitter refuses to announce a switch whose
origin and destination references differ.

The `route_switch` event it emits is deployment-scoped and cannot be anything
else: the fields that describe a substitution — `requestedModelId`,
`fromModelReference`, `toModelReference`, `authorizedByPolicy` — are not set,
and the function that builds the event takes no argument from which they could
be.

**A switch is only possible while nothing has been streamed.** Once output has
reached the customer, retrying elsewhere would deliver the beginning of one
answer and the whole of another, so the emitter refuses — and because the
executor asks the emitter rather than keeping its own copy of the rule, there is
one place that knows it. That is also why the `route_switch` event **precedes**
the `start` event: the switch really did happen before anything was streamed,
and the contract specifies event shapes without specifying their order.

**A switch is announced at the attempt that replaces the failed one**, not at
the moment of failure — the replacement's own breaker may refuse it, and
announcing early would tell a customer their request moved somewhere it never
went, and put a switch on the receipt that never happened.

**What is never retried elsewhere:** a request the provider could not express (a
refusal about the request, identical everywhere — retrying would make what a
request *means* depend on which route happened to be healthy), a content filter,
a cancellation, and any failure no adapter classified. One function decides,
`provider.AttributableCategory`, and the circuit breakers read the same one, so
the two can never drift apart.

### The policy Pensara is not sent, and what it does about it

**Failover is off by default, and that default is a contract finding rather than
caution.** The published `routingFallbackPolicySchema` gives the customer two
booleans that govern exactly this feature — `disabled` and
`sameModelDeployment` — and `routingPolicySchema` adds `allowedRegions` and
`deniedRegions`, which govern where a request may be served at all. The envelope
carries a routing policy **reference** and none of those values. Pensara therefore
cannot tell a customer who asked for failover from one who switched it off, and
failing over anyway would silently override a control the platform advertises.

So with no authorisation, a reference resolves to its **declared primary
deployment and nowhere else** — exactly how this build behaved before failover
existed. Choosing among deployments at all is the policy decision, so health
ordering is withheld too, not just the retry.

`PENSARA_ASSUME_FAILOVER_AUTHORIZED=<reason>:<YYYY-MM-DD>` turns it on. It is
deliberately awkward: it states that every caller of this process has a routing
policy permitting same-model failover across every deployment in its inventory,
which is true of a first-party canary and of nothing else. An empty value, a
bare `true`, or a reason with no date either leave the default in place or
refuse to start — never enable it. See item 11 below, which is the argument for
the snapshot travelling.

## Circuit breakers and health scoring

There are two rotations in this repository and they are different axes. This one
takes a DEPLOYMENT out when the deployment is failing. The other takes a
CREDENTIAL out when a provider has said something about that credential, and the
deployment goes on being served by the next key in the pool — see "Several
providers, and a key pool for each".

One breaker and one health score per **deployment**. The unit is the deployment
rather than the provider because a provider is usually several deployments in
several regions, and taking all of them out because one is failing throws away
the capacity failover exists to use.

| State | Meaning |
|---|---|
| `closed` | admitting requests |
| `open` | out of rotation until its cooldown expires |
| `half_open` | the cooldown expired; one real request at a time decides its fate |

**What trips one:** only a failure attributable to the deployment — the upstream
refusing, timing out, rate limiting, exhausting a quota, or rejecting *Pensara's
own* credential. Three consecutive ones open it, and the count is consecutive
rather than a rate because a rate needs a window and a window needs a traffic
assumption.

**What must never trip one:** a request the provider cannot express, a content
filter, a client that hung up. Those fail identically everywhere, so counting
them against a deployment would let one customer's malformed traffic take a
healthy route out of rotation for everybody — a denial of service with extra
steps. `Permit.NotAttributable` is how a caller says so rather than defaulting
into it, and it is mutation-tested from both directions.

**What probes it back in:** a cooldown, then **one real customer request**. Not
a burst — half-open admits exactly one trial at a time, because everything that
arrives the moment a cooldown expires is a thundering herd onto the provider
that just stopped failing. And not a synthetic probe: a synthetic probe proves
the provider answers some *other* request than the one it is failing, and Pensara
would be paying for it. A successful trial closes the breaker; a failed one
reopens it with a doubled cooldown, capped, so a long outage is still retried
within a bounded time.

The **health score** is an exponential moving average of attributable outcomes,
and it orders candidates: admitting breakers first, healthier before flakier,
the inventory's declared order as the tie-break. A deployment nothing has routed
to scores 1 — assuming the worst would sort it permanently last, and it would
never receive the traffic that would prove otherwise.

When every deployment of a model is out of rotation the request is refused with
`deployment_unavailable`, carrying a retry hint that is **the moment the
earliest breaker will admit its next trial** rather than a number chosen to look
reasonable.

## Several providers, and a key pool for each

A provider slug in the inventory resolves to an adapter, an address and a pool
of credentials. None of those three is in the inventory: a credential there
would be a copy of an Oxy entity, and an address there would make one process's
reachability a global fact. The inventory names the slug; `cmd/pensara` resolves
it.

**One protocol serves several providers.** OpenAI, OpenRouter and Cerebras all
speak OpenAI Chat Completions, so all three are a `Config` and a base URL rather
than three adapters — which is the claim `TestOneProtocolServesSeveralProviders`
has been making since that adapter was written, now with the wiring to match.
The Messages API is refused under any slug but `anthropic`: that adapter reports
its slug as a constant, so serving it under another name would attribute every
event and every usage record to a provider the inventory did not route to.

**A credential is a pool, not a value.** One provider account's capacity is not
one provider's capacity. When the account behind a key has nothing left, the
next key is a different account that does, and the request is served without the
customer learning anything happened.

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
| `exhausted` | the provider reported that this key's account has nothing left | the key is retired **and the request moves to the next key** |
| `rejected` | the provider refused this credential: revoked, invalid, or lacking access | the key is retired and the request does **not** move |
| `request_fault` | the request is what was refused | nothing is retired and nothing is retried |

**An exhausted key rotates and a refused one does not**, and the asymmetry is
the point. Exhaustion is expected and benign: the next key is a different
account and will very likely serve the request. A refused credential is a
configuration fault or a provider-side auth failure, and under the second every
remaining key is refused identically — so walking the pool would multiply one
failure into a call per key AND retire the whole pool on a blip. The two are a
matched pair in the conformance suite: a build that never rotates fails one, a
build that always rotates fails the other.

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
window (`PENSARA_PROVIDER_<SLUG>_KEY_RETIREMENT`, default 15 minutes) rather than a
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
Pensara cannot tell which it holds, so it does not guess:
`PENSARA_PROVIDER_<SLUG>_KEYS_ON_SEPARATE_ACCOUNTS=true` states it. Even then a
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

**It is not a route.** A rotation stays inside one deployment: same provider,
same endpoint, same upstream model, same weights, a different credential.
Nothing the customer was told about their route has changed, so no `route_switch`
is emitted and `routeSwitches` stays zero — asserted in the conformance suite,
which fails on a route switch appearing at all.

It therefore needs no routing-policy authorisation and does not touch
`PENSARA_ASSUME_FAILOVER_AUTHORIZED`. Choosing among the DEPLOYMENTS of a model is
governed by `routingFallbackPolicy.sameModelDeployment` and by
`allowedRegions`/`deniedRegions`, none of which the envelope carries; choosing
among the credentials of one deployment is governed by nothing published,
because there is nothing about it for a customer to have an opinion on. They are
different axes and the default of one is not weakened to make the other work.

**A refused credential is not retried on another deployment of the same
provider either.** The refusal is attributable, so the breaker takes the route
out of rotation and a failover to a deployment holding a DIFFERENT credential
stays possible — but two deployments of one provider slug resolve to one adapter
and therefore to one pool, so within a slug there is no different credential to
reach. Failing over there would reproduce, one deployment at a time, exactly the
walk the pool refuses to make: a key burnt per deployment on a single
provider-side authentication blip. The executor reads the same
`CredentialVerdictFor` the pool does, so the two cannot come to disagree, and a
candidate served by a different provider is still tried.

**A rotation can only happen before anything has been streamed.** The walk lives
entirely in front of the response body: not one byte has reached the customer,
so moving to another credential replaces the request instead of splicing two
answers together. Once a body is being read the request is committed to the key
that opened it — which is why a failure arriving mid-stream, after a 200, rotates
nothing. It is the same rule the executor applies to a route switch, arrived at
for the same reason.

**A request makes at most as many upstream calls as the pool has keys**, because
a key is never leased twice for one request. Nobody configured that ceiling; it
is a property of the walk.

**No credential enters anything.** Not a `Call`, an error, a log, a usage record
or the health projection. A key's identity outside `internal/provider` is its
1-based POSITION in the declared list — not a truncated hash, because a
fingerprint of a secret confirms a guessed secret and a position names a key
just as well. `GET /internal/v1/health` reports, per provider, how many
credentials are declared, how many are usable, and which are out and until when;
a provider answering a probe with part of its pool spent reports `degraded`
rather than `ok`, because a pool draining towards empty is otherwise invisible
until the request that finds it empty.

## Configuration snapshots

Pensara's configuration arrives as a file the control plane publishes. If that
pipeline stops, the data plane must not stop serving — and must not start
pretending it knows things it no longer knows. Those are two requirements, and
`inventory.Store` keeps them apart.

**A failed reload changes nothing.** The last good snapshot stays installed,
whole: a half-parsed inventory is never swapped in, so there is no state where
some references resolve and others silently vanish.

**What a stale snapshot may serve: any pinned reference, at any age.** The
mapping from immutable weights to a provider's model id cannot go stale, and a
pinned request is served or refused, never substituted — so the customer is told
exactly which weights answered, as always.

**What it may not serve: the choice of a current revision.** Which revision an
unpinned reference resolves to is Oxy's decision and it is the one thing in the
file that decays. Past the horizon (`PENSARA_INVENTORY_MAX_AGE`, default one hour)
an unpinned reference is refused with `service_unavailable`, retryable, naming
the age and saying that a revision-pinned reference is still served. Guessing
instead would serve weights Oxy may have replaced hours ago on a decision nobody
made.

**Prices do not enter this** — Pensara holds none — which removes the hardest half
of the usual stale-configuration problem. The only thing that decays here is a
routing choice, and it degrades rather than breaking.

**One requirement this places on the publisher:** staleness is measured from the
snapshot's own `issuedAt`, not from when Pensara last read the file. That is the
only measure that survives the failure that matters — a publisher that has
stopped running leaves a perfectly readable file on disk, and re-reading it
every thirty seconds would report it fresh forever. So the snapshot must be
**re-issued on a cadence shorter than the horizon even when nothing has
changed**. An unchanged snapshot with an old `issuedAt` is indistinguishable,
from here, from a control plane that has stopped publishing, and is treated as
one.

`GET /internal/v1/health` reports the snapshot id, its age, the horizon, whether
unpinned references are still being resolved, and the last reload failure with
no filesystem path in it.

## Publishing the inventory

`cmd/pensara-publisher` is the publisher the section above describes a requirement
for. It is a **separate process** from the one that serves, it asks the
providers themselves what they serve, and it re-issues the snapshot on a cadence
inside the horizon whether or not anything changed.

### Why it is a second command and not a goroutine

Writing the inventory decides which deployment every model reference resolves
to. That is a much larger authority than serving one request, and it is the
whole reason for the split: the permission to write the object belongs to a task
role that does nothing else, rather than to the role the serving process runs
under. `sts:AssumeRole` into a narrow role would not have achieved it — the
permission to assume would sit on the shared role, so every task holding that
role could assume it too.

### Where each field comes from

| Field | Source |
|---|---|
| `provider` | `PENSARA_PROVIDERS`, filtered to the slugs that hold a credential |
| `upstreamModelId` | that provider's own `GET /models`, verbatim |
| `modelReference` | `configs/model-attribution.json` for the publisher namespace, plus the observation date |
| `current` | true; each model line has exactly one revision, its observation |
| `deploymentId` | derived from the three above, so an unchanged re-issue keeps its ids |
| `issuedAt` | the clock, every cycle |
| `snapshotId` | a hash of the routing CONTENT, so it moves only when routing does |

The last two are deliberately different clocks. An operator asking "is the
publisher alive" reads `issuedAt`; asking "did routing change" reads
`snapshotId`. One value answering both would move every cycle and answer
neither.

### The revision label is an observation, and it is carried forward

These providers expose no immutable revision handle — the ids are bare aliases
and `created` is 0 — so a reference pins `@observed-<date>`: the date this
publisher FIRST saw the alias. That date is read back out of the previously
published snapshot on every cycle and reused forever.

Recomputing it would re-point every reference a customer has pinned, every day,
with everything green. So the previous snapshot is this job's only state, and a
read that FAILS is not treated as a first run: the cycle refuses rather than
re-date. Only a genuine 404 — nothing published yet — mints today's date.

### What it refuses, and what it merely drops

| Condition | Result |
|---|---|
| a declared provider holds no credential | dropped from the snapshot, warned; naming it would refuse its references or pin a permanent `unconfigured` alarm |
| a provider serves a model nobody attributed | dropped, warned; inferring a publisher from a model id is a claim about somebody else's work |
| one provider cannot be asked | its routes are absent for that cycle; the others still publish |
| no provider could be asked | the cycle refuses and the published snapshot is left alone |
| the previous snapshot cannot be read | the cycle refuses rather than re-date every reference |
| `PENSARA_INVENTORY_BUCKET` is empty | refuses to start, naming the variable; there is no default, because publishing to a guessed bucket succeeds silently |
| a cadence at or past the horizon | refuses to start rather than clamping |
| a provider speaking no `GET /models` | refuses; a hand-written list is the checked-in file this command replaces |

### Ordering is load-bearing

Failover is off by default, so a reference resolves to the deployment declared
FIRST and no other. Deployments are emitted in the order `PENSARA_PROVIDERS`
declares their providers, and only providers holding a credential are emitted at
all — so every declared route is servable, and the operator's provider list is
the one place a primary is chosen.

Two providers of one model line produce ONE reference with two endpoints, which
is the failover set. That is why the observation date is keyed by model LINE and
not by provider: keying it per provider would mint two revisions of one line,
which the reader refuses outright as two `current` revisions.

### The half of this that is Oxy's

`configs/model-attribution.json` maps a provider's own model id onto a canonical
`<publisher>/<model>` line. That is model IDENTITY, which ADR 0006 assigns to
**Oxy** — while `upstreamModelId` and `current`, in the same file, are execution
and are Pensara's. The inventory is the one artefact that has to carry both, so
neither side owns it outright.

It lives here because ADR 0006's "What crosses the boundary" declares exactly
one Oxy→Pensara channel, the per-request envelope, and no channel by which a
catalogue could publish an inventory. The response is to hold the smallest
possible amount of it, declaratively, and never to derive it: an unattributed
model is dropped rather than guessed at. If Oxy grows such a channel, that file
is what it replaces and nothing around it moves.

### Running it

Everything `cmd/pensara` reads about providers, this reads too, from the same
variables through `internal/providerconfig` — so the two commands cannot
disagree about where a provider lives. It uses ONE key from a pool: listing
models is a single unmetered call whose failure means "ask again later", and the
serving process owns the rotation.

| Variable | Required | Meaning |
|---|---|---|
| `PENSARA_PROVIDERS` | yes | the slugs to ask; the same list the serving process reads |
| `PENSARA_PROVIDER_<SLUG>_API_KEY` | yes, per slug | a slug with no key is dropped with a warning |
| `PENSARA_INVENTORY_BUCKET` | yes | the S3 bucket to publish into; never defaulted |
| `PENSARA_INVENTORY_KEY` | yes | the object key, e.g. `inventory/current.json` |
| `AWS_REGION` | yes | the bucket's region |
| `PENSARA_PUBLISH_INTERVAL` | no | re-issue cadence, default `15m`; refused at or past `PENSARA_INVENTORY_MAX_AGE` |
| `PENSARA_PUBLISHER_ATTRIBUTION_PATH` | no | default `/etc/relay-publisher/model-attribution.json`, baked into the image |

Credentials come from the ECS task role — `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI`
or `_FULL_URI`, refreshed before expiry — falling back to
`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_SESSION_TOKEN`. Neither is
defaulted into existence; with neither present the signer refuses and names what
is missing.

The image carries both binaries. A publisher task overrides `entryPoint` to
`/usr/local/bin/relay-publisher`; forgetting the override starts `relay`, which
refuses to boot without a snapshot, so the mistake is loud.

`internal/awssig` is a hundred lines of SigV4 rather than the AWS SDK, because
this module has no dependencies and needs one `GET` and one `PUT`. It is checked
against AWS's own published `get-vanilla` test vector, not against a second
reading of the specification by the same author.

## Provider cost

What the upstream will invoice Pensara for a request. It is an **operator**
number, and this is the only package in the repository that holds an amount of
money at all.

It is deliberately not the contract's money type. ADR 0006 gives Oxy every
customer-facing amount and Pensara its own upstream cost; `internal/contract` has
no money type and must not acquire one, so the two cannot be confused by
reaching for the same struct. Nothing here appears in any produced shape: the
stream events, the usage report and the error body have no field it could
occupy, and the descriptor gate fails on any field added to them that the
contract does not have. The containment check is the same amount in two places —
present in the operator log, absent from every byte the customer receives — with
a control proving a non-zero cost was measured, so "no cost in the response"
cannot be what an unpriced request also reports.

**A failed failover attempt is off the customer's receipt and on Pensara's cost.**
The customer never received that output, so charging for it would be wrong; the
provider invoices for it regardless, so dropping it would leave Pensara
reconciling against a number short by exactly its own failover traffic. That
asymmetry is why this is a separate measurement rather than a field on the usage
report.

**An unknown cost is not a zero cost.** A deployment with no rate card, or a
unit a card does not price, produces a measurement that says so and names the
unpriced units. Summing unknowns as zero yields a reconciliation that looks
complete and is quietly short by exactly the traffic nobody priced.

Rate cards are optional (`PENSARA_PROVIDER_RATES_PATH`), live in their own file
read by their own package, and are keyed by deployment id. Amounts are integers
in 1e-12 of the currency's major unit — the same scale as the published
contract's money type, so an operator reconciling an invoice against the ledger
is comparing like with like.

## The provider adapter interface

```go
type Adapter interface {
	Provider() contract.ProviderSlug
	Translate(request *contract.Request, route Route) (*Call, error)
	Stream(ctx context.Context, call *Call, out Emitter) (Outcome, error)
	Health(ctx context.Context) Health
}
```

- **`Provider`** names the slug every event and usage record attributes work to.
  It comes from the adapter, not its registration site, so a mis-registration
  cannot mislabel a receipt.
- **`Translate`** is pure. A request the provider cannot express is refused
  before anything is spent upstream, and a pure translation is testable with no
  network — which is what makes covering that refusal cheap.
- **`Stream`** is execution, streaming, cancellation and usage measurement
  together, because they share one lifetime: the units are only correct if they
  come from the same read loop that saw the last frame before the cut.
- **`Health`** must be answerable without a customer request, so it cannot be
  folded into `Stream`.

What an adapter deliberately does **not** do: allocate ids, assign sequence
numbers, decide terminality, emit `done`/`error`/`route_switch`, resolve a model
reference to an upstream model id, apply routing policy, or decide what a
failure means for the CREDENTIAL that produced it. That last one is
`provider.Walk` and `provider.CredentialVerdictFor`: an adapter says which
provider error this was, in the contract's vocabulary, and the pool decides what
follows — because "this key is spent" and "this key is refused" are opposite
decisions about a pool, and an adapter free to restate them is free to restate
them differently. Those are one
implementation in the executor, not one per provider. Adapters report semantic
content through `Emitter`, which stamps `requestId`, `sequence` and
`schemaVersion` itself — removing the whole class of bug where one provider's
events are unattributable or repeat a sequence.

## The ported adapters

Two protocols are implemented, and the second one exists to test the first one's
abstraction rather than to add a provider.

### `openaicompat` — OpenAI Chat Completions

A port of Alia's `openai` provider
(`packages/api/src/internal/providers/lib/providers/openai.ts`).

**Why that one.** Seven of Alia's adapters — openai, together, xai, cerebras,
hyperbolic, digitalocean, openrouter — are byte-identical apart from a base URL
and the word in their error string, because they all speak the OpenAI Chat
Completions protocol. Porting the *protocol* rather than a provider makes the
next six a `Config` and a conformance registration.
`TestOneProtocolServesSeveralProviders` runs the full suite under three more
slugs to keep that claim honest.

**What the port deliberately changes.** Alia's `proxy()` returned the upstream's
raw stream to its caller — no normalization, no usage, no cancellation, no error
classification. It also never sent `stream_options.include_usage`, so **a
streamed request reported no usage at all**; a faithful port of that would be a
billing hole. And it substituted `temperature: 0.7` / `max_tokens: 8192` when the
caller set none, which silently changes every request nobody configured.

### `anthropic` — the Messages API

A port of Alia's `anthropic` provider
(`.../providers/anthropic.ts`), and the answer to a question one adapter cannot
settle: whether `provider.Adapter` describes a provider or describes the first
one written against it.

**Why that one.** It disagrees with chat completions on every axis the interface
names — named SSE events instead of one repeated frame closed by `[DONE]`;
indexed content **blocks** whose kind is declared once in the event that opens
them; reasoning as a block type rather than a field; usage split across two
events with a **cumulative** output count; a failure that can arrive *inside* the
stream after a 200; `x-api-key` with a mandatory `anthropic-version` instead of a
bearer token; the system prompt hoisted out of the message list; a tool result
carried as a user message; and `max_tokens` **required**. A second
OpenAI-compatible provider would have exercised none of that.

Its usage fields also nest the *other way round*, which is the finding with money
attached: `input_tokens` **excludes** cached tokens where an OpenAI-compatible
`prompt_tokens` includes them, while `output_tokens` includes reasoning exactly
as `completion_tokens` does. So one of the two normalising subtractions the
contract's partition needs applies here and **the other must not** — and an
adapter written by copying the first one would under-report every cached request.

**What the port deliberately changes.** Alia's conversion read only a text delta
and `message_stop`: tool calls, reasoning, stop reasons and the whole of `usage`
were dropped, so a request that called a tool produced no tool call downstream
and every request reported no usage at all. It also defaulted `max_tokens: 8192`
and `temperature: 0.7`, and forced `stream: true`.

**What it refuses rather than inventing.** `max_tokens` is required upstream and
optional in the contract, so a request that omits `maxOutputTokens` is refused
with the field named. Choosing a ceiling here — or per deployment, which only
moves the invention into a config file — would truncate an answer the customer
asked to be unbounded and report success. That is item 14 below.

**No live provider call has been made from this repository.** There are no
provider credentials here, in the tests, or in CI. Both adapters are exercised
against a fake upstream that speaks the real wire format, including its habit of
echoing the request's credential header back inside an error message.

### What the second adapter changed

The `Adapter` interface itself did not change: `Provider`/`Translate`/`Stream`/
`Health`, `Call`, `Route` and `Outcome` all held. Three things around it did, and
each was a gap rather than a preference:

- **`Emitter` has nowhere to put provider-opaque block metadata.** A thinking
  block's `signature` is what makes multi-turn tool use with reasoning work, and
  no contract stream event has a field for it. The adapter reads it so it cannot
  be mistaken for output, and drops it. Item 17.
- **The conformance suite could only be told about one refusal**, which was an
  accident of the first adapter having exactly one. It now takes a list.
- **Credential redaction could not be left to the contract's pattern.** It is
  keyed to bearer-token shapes; against `x-api-key: <value>` it matches the
  marker and not the value, so redacting *removes the evidence and keeps the
  credential*. `provider.RedactSecret` removes the adapter's own key by exact
  match first. Item 18.

## The conformance harness

`internal/provider/conformance` is the suite an adapter must pass. An author
supplies five things — how to build the adapter, how to start a fake upstream
speaking that provider's **real** wire format, the route it serves, the requests
the provider genuinely cannot express, and what its fake upstream physically
consumed and produced — and gets back:

slug validity and stability · event framing (one `start` first, monotonic
sequences, `requestId` and `schemaVersion` on every event, exactly one terminal)
· a revision-pinned resolved model · the same normalized shape from a
non-streamed upstream · **units that partition the request**, on both read paths
· a provider that reports no usage settling as an estimate · tool calls a client
can reassemble · a transient throttle classified retryable · an exhausted
account classified non-retryable · **a refused PLATFORM credential classified
non-retryable and still attributable** · **a failure that arrives after the
response started** · **the configured credential never reaching the customer**, with a
control asserting the upstream actually echoed it AND that the customer still
receives the upstream's diagnostic rather than losing it to the contract's
refusal · one refusal per class, each
spending nothing upstream and naming the field at fault · **an exhausted
credential served by the next key in the pool**, with no route switch and the
units still partitioning the request · **a refused credential NOT walking the
pool** · **a request the provider itself refused retried nowhere** · **each
credential spent at most once** on a pool where every key is exhausted ·
cancellation, with its control · health with and without a credential.

The author now supplies at least TWO credentials rather than one. A pool of one
cannot tell an adapter that rotates on exhaustion from one that cannot rotate at
all, and two keys that are the same string cannot tell "the second key served
it" from "the first key was retried" — both are refused by the suite before any
of its checks run.

The suite drives the adapter through the **real executor**, because an adapter is
only correct in the shape it is actually used.

**What the second adapter changed here, and which half of it was general.** Six
changes, and the distinction matters:

| Addition | General, or the suite having been OpenAI-shaped? |
|---|---|
| Units partition the request (`StreamedUsage`) | **General.** It is the contract's own rule, and it caught the double-charge on the *first* adapter when that adapter's subtraction was removed — the suite had no check that would have. |
| A failure arriving mid-stream, after a 200 | **General.** Both protocols can do it; neither adapter handled it before, and `openaicompat` was reporting a truncated answer as a completed one. |
| A refused platform credential (`provider_credential_invalid`) | **General**, and newly expressible: the code landed in `@oxyhq/contracts@0.28.0` while this branch was open. Both adapters were reporting it as retryable. |
| A list of refusals rather than one | **The suite was OpenAI-shaped.** One slot fit because the first adapter had exactly one refusal class. |
| `maxOutputTokens` populated in the fixture | **The suite was OpenAI-shaped**, in the sense that a minimal fixture only passes for a provider that requires nothing the contract makes optional. Populating optional fields is this repository's own rule anyway. |
| "an exhausted quota on the *same status*" | **Was OpenAI-specific prose.** The invariant is that an adapter tells a throttle from an exhausted account; that they share a status is one provider's habit. Wording only. |

## Running it

Everything comes from the environment and one inventory file. There is no
unauthenticated mode, not even for local development: a bypass that exists is a
bypass that ships.

| Variable | Required | Meaning |
|---|---|---|
| `PENSARA_INVENTORY_PATH` | yes | deployment inventory snapshot (see `configs/inventory.example.json`) |
| `PENSARA_EDGE_PUBLIC_KEYS` | yes | `kid:base64,…` Ed25519 **public** keys; not secret |
| `PENSARA_PROVIDERS` | yes | the provider slugs this process serves, e.g. `openai,openrouter,cerebras,anthropic` |
| `PENSARA_PROVIDER_RATES_PATH` | no | upstream rate cards; absent ⇒ provider cost is not measured |

Then, per provider, with `<SLUG>` upper-cased and `.`/`-` replaced by `_`. Two
slugs that collapse onto one variable name are refused at startup rather than
silently sharing an address and a pool.

| Variable | Required | Meaning |
|---|---|---|
| `PENSARA_PROVIDER_<SLUG>_PROTOCOL` | for an unknown slug | `openai_compatible` or `anthropic_messages` |
| `PENSARA_PROVIDER_<SLUG>_BASE_URL` | for an unknown slug | the provider's API root |
| `PENSARA_PROVIDER_<SLUG>_API_KEY` | no | one credential, or a pool separated by commas; absent ⇒ the provider reports `unconfigured` |
| `PENSARA_PROVIDER_<SLUG>_HEADERS` | no | `Name=Value` pairs the provider expects, comma-separated (OpenRouter's attribution headers) |
| `PENSARA_PROVIDER_<SLUG>_KEY_RETIREMENT` | no | how long a spent or refused key stays out, default `15m` |
| `PENSARA_PROVIDER_<SLUG>_KEYS_ON_SEPARATE_ACCOUNTS` | no | `true` when the pool's keys are DIFFERENT provider accounts; only then does a throttle rotate. Anything but `true`/`false` refuses to start rather than quietly meaning "not set" |

`openai`, `anthropic`, `openrouter` and `cerebras` carry a built-in protocol and
published API root, both overridable. Any other slug declares both: an address
this build guessed would be an address nobody chose. A wrong address fails loudly
on the first request, which is why a default here is not the kind of invention a
default sampling parameter would be — no live call has been made from this
repository to any of them.

This block shows the SHAPE of the per-provider variables. It is not a
deployment, and the provider list in it is not the one to deploy — see
[What the deployment must supply](#what-the-deployment-must-supply-and-what-happens-when-it-does-not),
which declares only the slugs whose key exists.

```bash
# ILLUSTRATIVE — shows the variable shapes, NOT a deployable provider set.
# Declaring a slug with no key starts cleanly and serves nothing: the adapter
# reports `unconfigured` and refuses every request routed to it. Deploy only
# the slugs whose key exists.
PENSARA_PROVIDERS=cerebras,openrouter,openai
PENSARA_PROVIDER_CEREBRAS_API_KEY=…
PENSARA_PROVIDER_OPENROUTER_API_KEY=…,…,…          # a pool of three
PENSARA_PROVIDER_OPENROUTER_KEYS_ON_SEPARATE_ACCOUNTS=true
PENSARA_PROVIDER_OPENROUTER_HEADERS=HTTP-Referer=https://oxy.so,X-Title=Oxy
PENSARA_PROVIDER_OPENAI_API_KEY=…
```

**The one closed list here is the PROTOCOL.** It names which adapter
implementation to construct, and a build can only construct one it contains, so
an unknown value is refused rather than defaulted. Provider **slugs are not a
closed list**: the built-in table is defaults, and any slug that declares a
protocol and a base URL is servable with no Go change.

### What these names have to agree with

The variable name is also the SSM parameter leaf and the GitHub secret name, and
the three are one string because the deploy sync derives the parameter path from
the secret name — a design where they differ breaks the sync with no error.

The slug→variable transform is total, and its output is always a legal
environment variable name: a slug is `[a-z0-9._-]`, the two characters a
variable name cannot carry are folded to `_`, and the constant prefix means the
result never begins with a digit. What the folding creates is **collisions** —
`open-router` and `open.router` are two slugs and one variable name — and those
are refused at startup rather than resolved, because the loser would silently be
configured with the winner's address and credentials.

**A whole pool lives in the one `_API_KEY` variable**, not in numbered siblings.
That is a deployment property rather than a preference: credentials are
delivered as a static list resolved once at task launch, so a name per key would
grow that list with the pool and make a missing key inside one an invisible
deploy-time edit. One name per provider keeps adding a KEY a parameter *value*
change, and adding a PROVIDER one new name. A credential containing a comma is
not representable, and a blank or duplicated entry is refused where it is read.

### The three things that move on different clocks

The published snapshot, this build's adapter set and the deployment's credential
list are updated by different people at different times, and each pairing fails
differently. None of them is fatal, and one of them is invisible from outside:

| State | What happens |
|---|---|
| a provider is declared with no credential | the process starts and the adapter reports `unconfigured`; a request to it is refused non-retryably, naming the operator's gap |
| the snapshot routes to a provider this build has no adapter for | a startup **warning** naming it; references served only by that provider are refused, every other reference is served |
| a credential is delivered for a provider that was never declared | a startup **warning** naming the variable — the only signal there is |

The second used to refuse to start. It no longer does, because the inventory is
published by the control plane while the adapter set is fixed at deploy time: a
provider can appear in a snapshot before the deploy that gives this build its
credential, and stopping there would take routing for every SUPPORTED provider
down over one unsupported one — on the next task replacement rather than when
the snapshot changed, since the reload path already treated the identical
condition as a warning. Two answers to one question, and the fatal one was
reachable only by restarting.

The third is the one worth stating plainly, because **no check outside the
process can see it**: the task starts, the health probe passes, the rollout
reports complete, and the provider is simply absent. The environment and the
provider list are only both visible here, so this is where it is named. It
warns rather than refuses — retiring a provider means removing it from the list
before deleting its parameter, and a refusal would turn the safe order into the
one that stops every task.
| `PENSARA_INVENTORY_MAX_AGE` | no | staleness horizon for unpinned resolution, default `1h` |
| `PENSARA_INVENTORY_RELOAD_INTERVAL` | no | default `30s` |
| `PENSARA_ASSUME_FAILOVER_AUTHORIZED` | no | `<reason>:<YYYY-MM-DD>`; absent ⇒ no failover, see above |
| `PENSARA_BREAKER_FAILURES_TO_OPEN` | no | default `3` |
| `PENSARA_BREAKER_COOLDOWN` | no | default `5s` |
| `PENSARA_BREAKER_MAX_COOLDOWN` | no | default `2m` |
| `PENSARA_BREAKER_SUCCESSES_TO_CLOSE` | no | default `1` |
| `PENSARA_ADDR` | no | default `:8080` |
| `PENSARA_EDGE_MAX_SKEW` | no | default `5m` |
| `PENSARA_MAX_ENVELOPE_BYTES` | no | default `16777216` |

```bash
go build ./... && go vet ./... && go test -race ./...
golangci-lint run ./...
cd tools/contract && npm ci && npm run generate && npm run validate
```

## Deploying it

`Dockerfile` builds a `linux/arm64` image — Oxy's ECS cluster is Graviton —
and `.github/workflows/deploy-aws.yml` pushes it to ECR, registers a task
definition naming that image by DIGEST, and repoints the service at the new
revision. A tag would not do: `--force-new-deployment` against `:latest`
relaunches whatever the tag resolves to at that moment, so "what is running"
stops being a question the task definition can answer.

The image is a stripped, statically linked binary on `distroless/static`,
running as uid 65532 with no shell and no package manager. It carries no
inventory, no rate card and no credential of any kind.

### The health path is `/livez`

It is the only route that answers without an Oxy edge signature, so it is the
only one a health probe can use. `/internal/v1/health` returns **401** to an
unsigned probe and `/health` does not exist — a check pointed at either marks
every task unhealthy and the service never stabilises.

**The probe has to come from outside the container.** The image carries no
shell, no `wget` and no `curl`, so neither a Dockerfile `HEALTHCHECK` nor an ECS
container `healthCheck` can run a command in it, and the ECS one fails by never
passing rather than by erroring. An HTTP check against `/livez` from a load
balancer or equivalent needs nothing in the image and works as it stands.

Without any probe, ECS still replaces a task that *exits*, and Pensara exits
non-zero on every startup failure reachable from configuration. What is
uncovered is "running but not serving". The alternative that would close that
gap without a shell is a self-probe flag on the binary, which does not exist
today and is a change to `cmd/pensara`, not to the image.

### What the deployment must supply, and what happens when it does not

**`PENSARA_PROVIDERS` is required and is not a secret.** It lists the slugs this
process serves, and an empty one is a hard refusal at startup rather than a
process that serves nothing quietly. It belongs in the task definition's plain
environment beside `PENSARA_EDGE_PUBLIC_KEYS`, and its value is **the slugs that
have a key**, not the slugs the platform intends to offer eventually:

```
PENSARA_PROVIDERS=cerebras
```

`openai`, `anthropic`, `openrouter` and `cerebras` carry a built-in protocol and
base URL, so a slug from that set needs nothing but its key. Any other slug must
also be given `PENSARA_PROVIDER_<SLUG>_{PROTOCOL,BASE_URL}`. The rest of the
per-provider surface — `_HEADERS`, `_KEY_RETIREMENT`,
`_KEYS_ON_SEPARATE_ACCOUNTS` — is non-secret too and goes in the same block.

**Do not declare a slug whose key does not exist yet.** An adapter with no
credential reports `unconfigured` for as long as it is declared, which is
exactly the condition worth alarming on when a key goes missing later — so
declaring ahead of the key pins that alarm on permanently and destroys it. A
signal that is always firing is not a signal.

Three constraints govern the set, and only the last is fatal:

```
PENSARA_PROVIDERS   ⊇ the providers the snapshot routes to   else those references are refused
PENSARA_PROVIDERS   ⊆ the providers that have a key          else a permanent `unconfigured`
secrets[]         ⊆ the SSM parameters that exist          else the task never starts
```

The first two together are a constraint on whoever publishes the inventory: a
snapshot may not name a provider whose key does not exist, because there is no
value of `PENSARA_PROVIDERS` that serves it without either refusing its references
or pinning an alarm on.

**And naming a servable provider is not enough — it has to be declared FIRST.**
Failover is off by default, so choosing among the deployments of one model is
withheld entirely and a reference resolves to the deployment the inventory
declared first and no other. A reference whose first declared deployment sits on
a provider this process does not serve is therefore refused even when a later
deployment of the same reference is one it does, and no health ordering rescues
it, because health ordering is withheld by the same default. So the requirement
on the snapshot is per REFERENCE and not per file: for every reference meant to
be served, a servable deployment has to be the first one declared. See "The
policy Pensara is not sent".

**Adding a provider runs in one order and retiring one runs in the reverse**,
both for the same reason — the third constraint above is the only fatal one.
Add: the repository secret and its name in the workflow's allow-list, then run
the workflow so the parameter exists, then the slug in `PENSARA_PROVIDERS` and the
parameter's name in `secrets[]`. Retire: out of `secrets[]` and
`PENSARA_PROVIDERS`, deploy, and only then delete the parameter. Deleting it while
it is still named stops every task rather than that provider's routes.

**Provider credentials are the only secrets**, one per declared slug. The
deployed set is a subset of these, tracking whichever keys exist:

| SSM parameter | Type | Env var |
|---|---|---|
| `/oxy/relay/PENSARA_PROVIDER_CEREBRAS_API_KEY` | `SecureString` | `PENSARA_PROVIDER_CEREBRAS_API_KEY` |
| `/oxy/relay/PENSARA_PROVIDER_OPENROUTER_API_KEY` | `SecureString` | `PENSARA_PROVIDER_OPENROUTER_API_KEY` |
| `/oxy/relay/PENSARA_PROVIDER_OPENAI_API_KEY` | `SecureString` | `PENSARA_PROVIDER_OPENAI_API_KEY` |

The deploy workflow's allow-list carries all three, because a name there with no
repository secret is skipped with a warning and is inert. The task definition's
`secrets` block must carry **only the parameters that exist** — a name there
without one fails the task at launch with `ResourceInitializationError`, and it
fails every task, not that provider's routes. The two lists are governed by
different constraints and are not meant to match.

**No list grows with a key pool.** A pool is comma-separated inside one
`_API_KEY` value, so widening or rotating one never touches the workflow's
allow-list, the task definition's `secrets` block, or this table. What moves
them is a slug arriving or leaving, and the order is not symmetric: **adding**
goes SSM parameter first, then `secrets[]`, then `PENSARA_PROVIDERS`, because each
step is inert until the one before it exists; **retiring** goes the other way —
out of `PENSARA_PROVIDERS`, then out of `secrets[]` and rolled out, and only then
delete the parameter, because a `secrets` entry outliving its parameter stops
every task.

**A credential for a slug `PENSARA_PROVIDERS` does not declare is inert**, and the
process says so at startup — it names the offending variables and tells you to
either declare the provider or drop the secret. Without that line it would be
the one failure with no downstream signal at all: the task starts, the probe
passes, the rollout completes and the provider is simply absent.

**An absent credential does not stop the process.** The adapter reports itself
`unconfigured` on `/internal/v1/health`, `/livez` still answers 200, and the
rollout therefore completes: the gap surfaces as a refused inference request,
not as a failed deploy. That is deliberate — an operator sees it on the health
surface before a customer sees it — but it means a green deploy is not evidence
that a provider is reachable. Conversely, an SSM parameter named in the task
definition and absent from Parameter Store fails at task launch with
`ResourceInitializationError: unable to pull secrets`, and the rollout fails.

**The Oxy edge key is a public key and belongs in plain environment**, never in
`secrets`. Pensara holds only public keys and cannot construct an envelope it
would itself accept; storing a signing key here would destroy that property.

```
PENSARA_EDGE_PUBLIC_KEYS=oxy-edge-2026-08-17:jQBxDX3B/Z0ULOHPbQz3gfFinKpl7Qv5MVBTfRYSd34=
```

**`PENSARA_ASSUME_FAILOVER_AUTHORIZED` is left unset**, which is the strict
setting: a model reference resolves to its declared primary deployment and
nowhere else. Setting it asserts that every caller of the process has a routing
policy permitting same-model failover, and the envelope carries nothing that
would let Pensara check that. It is not for a shared production deployment, and
`cmd/pensara` refuses to start on a bare `true` precisely so it cannot arrive as
one.

### The configuration snapshot is mounted, not baked

`PENSARA_INVENTORY_PATH` defaults to `/etc/relay/inventory.json` in the image, and
the image ships no file there. Baking one in would freeze its `issuedAt`: past
`PENSARA_INVENTORY_MAX_AGE` every unpinned reference is refused, so the deploy
would go green and start degrading an hour later. The snapshot has to be
re-issued on a cadence shorter than the horizon, which is a property of a
publisher, not of a file — see "Configuration snapshots".

So `/etc/relay` is a volume, and something publishes into it. **The publisher is
`cmd/pensara-publisher`, in this repository** — see "Publishing the inventory". It
writes one S3 object; what carries that object into the volume is a deployment
choice, and two mechanisms fit ECS Fargate:

- **A sidecar syncing from S3** into a task-scoped volume shared with the relay
  container. Pensara's own reload loop picks the file up within
  `PENSARA_INVENTORY_RELOAD_INTERVAL`. The sidecar should download to a temporary
  file and `rename(2)` over the destination: a reader that catches a partial
  write survives it from the second snapshot onward, but on the FIRST there is
  no last-good snapshot to fall back to and the process exits non-zero.
- **An EFS access point** mounted by both the publisher and Pensara.

Until one exists, the container exits non-zero at startup with
`inventory: reading /etc/relay/inventory.json: no such file or directory`, and
the rollout fails. That is the intended failure: a data plane with no inventory
routes nothing, and failing loudly beats serving `configs/inventory.example.json`,
whose routes and upstream model ids are illustrative and were never verified
against a real provider.

**`configs/inventory.json` is the first snapshot, measured by hand; it is not a
deployment.** Its two upstream model ids were read from the Cerebras API with
the account's own key rather than from documentation, and the publisher now
produces the same content from the same source on a cadence. Committed, the file
carries a frozen `issuedAt`, and an hour after that instant every unpinned
reference in it is refused while every pinned one is still served. That is the
whole difference between a snapshot and a publisher, and it is why mounting this
file verbatim is not a deployment.

`PENSARA_PROVIDER_RATES_PATH` is left unset unless a real rate card is published
the same way. Unset means provider cost is not measured, and every measurement
says so rather than reporting zero.

## Explicitly out of scope

Named here so nobody assumes otherwise. None of these is stubbed; each is simply
absent, and the code refuses rather than pretending.

- **Cross-model fallback.** Would require `routingFallbackPolicy`'s
  `authorizedCrossModel` list, which arrives only inside the policy snapshot
  Pensara is not sent. Nothing here can express it: the route-switch event Pensara
  builds is deployment-scoped by construction.
- **Failover without an operator acknowledgement.** Same-model failover is
  built, tested and off by default, for the contract reason above and in item 11
  below.
- **Reconciliation of provider cost against provider invoices.** Pensara measures
  what each request cost it upstream; matching that against what a provider
  actually billed is a finance process with no home in a data plane.
- **Oxy-hosted open-weight serving (vLLM/SGLang) and any GPU scheduler.** The
  epic says not to block the first API-only launch on a scheduler.
- **BYOK.** The provider-connection shapes are recorded not-applicable. The key
  pools are RELAY's own credentials with each provider; a customer's are an Oxy
  concept and stay one.
- **An official quota API or authenticated usage endpoint.** It would sit above
  the response-header mapping in the preference order and is not implemented:
  every provider spells one differently, none can be exercised from a repository
  with no credentials and no live provider call, and a poller written against
  documentation alone is the same guess in a more expensive form. Exhaustion is
  learned from the provider's own refusal instead, which is a stronger signal
  and arrives on the request that would have been refused anyway.
- **A threshold-based `approaching_limit` quota state.** It would need a
  threshold nobody has chosen, and nothing in this build would act on it: a key
  is usable or it is not.
- **Modalities other than text.** Embeddings, images, audio and rerank are
  refused with `unsupported_modality` rather than mistranslated.
- **Routing-profile targets**, for the contract reason below.
- **Replay protection beyond the signature time window.** Pensara keeps no nonce
  cache; the edge owns request idempotency.

## What Oxy still has to decide

These surfaced while implementing against the contract, which has never been
implemented against before. Each is a real gap, not a preference.

1. **The envelope carries a routing policy *reference*, not a snapshot.**
   `inferenceRequestSchema.routingPolicy` is `{routingPolicyId, policyVersion}`.
   But ADR 0006 assigns *routing execution* to Pensara and ADR 0010 says the
   envelope carries "the resolved routing policy snapshot and its version". As
   published, Pensara **cannot** enforce provider allowlists, region residency,
   zero-data-retention, licence constraints or price ceilings — it has no
   values to enforce. Either the envelope must carry the snapshot, or the ADRs
   should say plainly that Oxy enforces all of it and Pensara only executes.
   Item 11 is the sharpest instance of this and the one that blocks working
   code.
2. **A `routing_profile` target is unresolvable.** `routingTargetSchema` lets a
   request say "choose one for me", but the candidate list lives in the Oxy
   catalogue's `routingProfileSchema` and is not in the envelope. This build
   refuses such a request with `invalid_request` and `param:
   target.routingProfile` rather than picking a model, which would be exactly
   the silent substitution the platform forbids. Either Oxy resolves the profile
   before forwarding (in which case, why send the profile kind?), or the snapshot
   travels with the envelope.
3. **`requestId`'s owner is stated two different ways.** `identifiers.ts` says
   "a request id generated by the data plane", but it is *required* inside
   `attribution` on the inbound envelope, so it cannot be. ADR 0010 has the edge
   allocating it at step 1. Pensara implements the ADR's reading — Oxy allocates
   `requestId`, Pensara allocates `generationId` — and the comment should be
   corrected.
4. **No reservation or deadline in the envelope.** ADR 0010's `InferenceEnvelope
   v1` lists `reservation {reservationId, ceiling, priceVersion}` and `deadline`;
   `inferenceRequestSchema` has neither. So Pensara cannot enforce a spend ceiling
   or an execution deadline, and `normalizedUsageReportSchema` carries no
   `reservationId` (nor an `idempotencyKey`, though `usageReceiptSchema` has
   one) — Oxy must correlate settlement by `requestId` alone. Workable, but it
   should be a stated decision.
5. **`cached_input_tokens` and `reasoning_tokens` are not defined as subsets or
   siblings.** *Answered by OxyHQ/oxy#1019: the units **partition** a request,
   which is what Pensara already reported and what the ledger's arithmetic already
   assumed.* Two things follow that the answer does not cover, and the second
   adapter is where both surfaced.

   First, **the normalising subtraction is per provider and is not the same
   subtraction**. An OpenAI-compatible `prompt_tokens` includes its cached
   tokens; Anthropic's `input_tokens` is documented as excluding both of its
   cache counts, with the prompt total being
   `cache_read + cache_creation + input_tokens`. Copying the first adapter's
   arithmetic into the second would under-report every cached request. Both
   halves are now a conformance check rather than a comment.

   Second, **there is no unit for a cache WRITE**. Anthropic reports
   `cache_creation_input_tokens` separately and prices it at 1.25× to 2× the base
   input rate, against 0.1× for a read. Pensara folds writes into `input_tokens`,
   because the alternative — reporting them as `cached_input_tokens` — would
   price the most expensive input tokens in the request at the cheapest rate on
   the card. The units still partition the request; what is lost is the premium,
   on Pensara's own cost side. A `cache_write_input_tokens` unit would close it.
6. **The closed error set has no non-retryable platform-side failure.**
   *Answered by OxyHQ/oxy#1019, which added `provider_credential_invalid`
   (non-retryable), published in `@oxyhq/contracts@0.28.0` and adopted here.*
   Both adapters now report an upstream refusing the PLATFORM's credential under
   that code, with category `authentication`. The two halves pull in opposite
   directions on purpose: the code is non-retryable so a client stops hammering a
   request that cannot succeed, and the category is attributable so the breaker
   still takes the route out of rotation and a same-model failover to a
   deployment holding a DIFFERENT credential is still allowed. It is a
   conformance check, so the next adapter inherits it. Getting there also
   surfaced item 19, which is the larger of the two findings.

   **The neighbouring gap is closed too.** An upstream refusing to *bill* the
   platform is the same class of failure — only an operator can act, no retry
   helps — and reporting it as `quota_exceeded` was correct about retryability
   and wrong about whose account is exhausted, which reads as actionable while
   the action does nothing. `provider_billing_refused` landed in
   `@oxyhq/contracts@0.29.0`; Anthropic's 402 `billing_error` and an
   OpenAI-compatible `insufficient_quota` both map to it, and the conformance
   suite refuses any code that names the CUSTOMER's money for that scenario.
7. **Nothing specifies how Pensara authenticates the edge.** See
   `internal/edgeauth` for what Pensara implements and why it follows ADR 0012's
   asymmetric reasoning rather than a shared secret.
8. **The deployment descriptor has no upstream model identifier.**
   `modelDeploymentSchema` cannot express what a provider calls a model, so that
   mapping lives in Pensara's inventory. The same descriptor also carries
   `availabilityScope`, `commercialPermission` and `priceVersionId` — Oxy
   commercial decisions under ADR 0006 — so the shape currently has two owners
   and no stated direction of exchange.
9. **Nothing says who picks the current revision of an unpinned reference.** The
   contract says Oxy chooses it, but the envelope carries no resolution and the
   `start` event must report a revision-pinned reference — so in practice Pensara
   chooses. It does so from an explicit `current` flag in the inventory.
10. **Several produced shapes are not `.strict()`.** The stream events, the
    usage report and the error body all allow unknown keys, so a field Pensara
    emitted by mistake is silently stripped at Oxy's parse rather than caught.
    The request's `client` block *is* strict, and that strictness is what makes
    its privacy rule enforceable — the same argument applies to the rest.
11. **The customer's own switch for same-model failover never reaches the data
    plane that implements it.** `routingFallbackPolicySchema` carries
    `disabled`, `sameModelDeployment` and `authorizedCrossModel`;
    `routingPolicySchema` carries `allowedRegions` and `deniedRegions`. Every
    one of them governs what this repository's failover does, and the envelope
    carries none of them — only `{routingPolicyId, policyVersion}`. So a Pensara
    that failed over by default would override, for every customer who set it,
    a control the platform advertises to them. This build therefore ships
    failover **off**, and choosing among the deployments of one model at all is
    withheld with it, since that choice is governed by the same values. It
    cannot even be pre-implemented speculatively: adding a snapshot field to the
    Go request type fails the descriptor gate, because the published shape does
    not have one. **This is the concrete case for the snapshot travelling.**
    Failing that, Oxy should state that it resolves the deployment as well as
    the model, and send one — at which point Pensara's inventory and Oxy's
    catalogue need the direction of exchange that item 8 already asks for.
12. **The contract specifies event shapes and not their order.** Pensara emits
    `route_switch` *before* `start`, because the only switch it can safely
    perform is one where nothing has been streamed yet, and saying so in order
    is the truthful framing. The alternative reading — that `route_switch`
    amends a `start` already sent — is only expressible for a switch that
    happens mid-stream, which would duplicate output. If any consumer assumes
    `start` is always the first event, that assumption should become a stated
    rule in the contract rather than an implicit one.
13. **A model-scope `route_switch` cannot be constructed for a pinned request.**
    `routeSwitchDetail.requestedModelId` is the *unpinned* model line the
    customer asked for, so a request that pinned a revision has no value that
    satisfies the field. That is consistent with never substituting pinned
    weights, but it means cross-model fallback is expressible only for unpinned
    requests, which is worth stating rather than discovering.

The five below came from implementing the SECOND adapter. They are the ones a
single implementation could not have found, because each is a place where the
contract fits one provider's shape and not another's.

14. **`maxOutputTokens` is optional, and at least one provider requires it.**
    The Anthropic Messages API rejects a request with no `max_tokens`. The
    contract makes the field optional and says nothing about what its absence
    means, so an adapter has three options and two of them are wrong: invent a
    ceiling (silently truncates an answer the caller asked to be unbounded, and
    reports success), take one from the deployment descriptor (the same
    invention, moved to a config file nobody reads), or refuse. This build
    refuses, with `invalid_request` and `param: maxOutputTokens`, which means a
    request that is valid under the contract and served by one provider is
    refused by another. Either the contract should require the field, or
    `modelDeploymentSchema` should carry a per-deployment output ceiling and the
    contract should say that an absent value means it — but it should say which.

15. **There is no unit for cache-write tokens.** See item 5. A provider that
    prices a cache write at a premium and a cache read at a tenth of the input
    rate cannot be metered exactly against the published unit list.

16. **`refusal` had no finish reason of its own.** *Closed in
    `@oxyhq/contracts@0.29.0`.* The Messages API stops with
    `stop_reason: "refusal"` when the model declines; the contract's finish
    reasons ended at `content_filter`, so Pensara had to report a filter acting
    where the model had declined — different things to a customer deciding
    whether to rephrase, and a distinction the delta channels already carried.
    `refusal` is now a finish reason and this adapter emits it.

17. **No stream event can carry provider-opaque block metadata.** An
    extended-thinking response returns a `signature` per thinking block, and the
    provider REQUIRES those blocks back, unmodified, on the next turn of a
    tool-use conversation. `streamDeltaEvent` has `channel` and `text` and
    nothing that could hold it, and the request side has no content-part type
    for a thinking block either — so the round trip is not expressible in either
    direction, and multi-turn tool use with reasoning cannot be served through
    this contract at all. Pensara reads the signature so it cannot be mistaken for
    output, and drops it. This is a design decision rather than an oversight to
    patch: an opaque per-block blob crossing the boundary needs a home nobody has
    chosen yet.

18. **`safeErrorTextSchema`'s credential pattern was bearer-shaped, and
    redacting against it made a leak worse.** *Closed in
    `@oxyhq/contracts@0.29.0`.* The old pattern refused `authorization:`,
    `bearer <token>`, `api_key=` and `sk-…`. An upstream echoing
    `{x-api-key: <value>}` matched the **marker** and not the **value**, so
    redacting the match produced `{x-[redacted] <value>}` — which no longer
    tripped the refinement and was therefore *accepted* with the credential
    intact. The rewrite is four independent signals, one of which is a
    placeholder standing beside a surviving opaque value: the residue of exactly
    that span redaction.

    **Two things this repository has to keep doing, and they are the reason the
    item stays here rather than being deleted.** First, `SafeErrorText` no
    longer redacts a span — it withholds the whole message or none of it, since
    a span redaction is now both wrong and *refused*. Second, the published
    refinement is a last-resort refusal and says so: it cannot see a credential
    with no marker, no issued-token prefix and no placeholder beside it, because
    refusing those bytes means refusing request ids. `provider.RedactSecret`,
    applied by the adapter that still holds the bytes it sent, is the control —
    and it earns its place twice over. Where the pattern *does* recognise the
    shape, redacting the value is what keeps the customer's diagnostic instead
    of losing the whole message to the refusal; where it does not, redaction is
    the only thing between the credential and the customer.
    `internal/contract`'s fixture table pins both halves against the published
    schema itself, including a string that carries a secret and is accepted.

19. **A published version number did not identify the contract it names, and
    nothing gates that.** While this adapter was being written,
    `@oxyhq/contracts` on `main` and `@oxyhq/contracts@0.27.0` on npm had
    *different contents under the same version*: `main`'s `errors.ts` carried
    `provider_credential_invalid` and the published tarball did not, because
    #1019 merged after 0.27.0 shipped and did not bump the version. The
    immediate effect here was a code that could not be adopted; the lasting one
    is that "which 0.27.0 do you have" had no answer.

    **No in-repo consumer could have seen it.** Everything inside the monorepo
    resolves `workspace:*` and therefore reads `main`'s source; this repository
    is the first consumer that installs the published artefact, so the two
    copies had never been compared before. That is also why the fix belongs
    upstream rather than here.

    OxyHQ/oxy#1025 bumped and published `0.28.0`, closing the drift, and this
    build is generated against it. What is still open is the gate: nothing
    fails when a change to `packages/contracts/src` merges without a version
    bump, so the same divergence can reopen silently on any later PR. A CI check
    comparing the working tree's contract source against the published tarball
    for the version in `package.json` would answer it once.

[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/sdk/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/sdk/blob/main/docs/adr/0006-oxy-relay-boundary.md
