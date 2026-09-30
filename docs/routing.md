# Routing, failover and health

What Kaana does when a request is cancelled, when a deployment fails, and how it decides a provider is unwell.

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

## Control-plane selection, data-plane execution

Oxy selects only after applying customer policy. An explicit routing-profile
priority comes first, the reviewed score is descending within that priority,
and exact `deploymentId` code-unit comparison is the sole deterministic
equal-score tie-break. Provider name, model name, display name, insertion order
and database order never select a route.

Identity, price and score evidence are Oxy control-plane inputs. If any otherwise
eligible route has a missing exact ID, price or score; a stale score; a score for
another price version; or an ID duplicate/collision, Oxy refuses the whole set
before reserving spend and before calling Kaana. Kaana receives no score or
`optimiseFor` value and does not second-guess that decision.

## Authorized failover

Oxy sends `authorizedRoutes` in preference order after applying the customer's
routing policy. Kaana attempts that list in exactly that order and never adds an
inventory route, reorders by health, or interprets the policy reference. Before
execution it resolves every `deploymentId` against one inventory snapshot and
requires the signed provider, pinned model reference and region set to agree
exactly. Empty equals empty and means no regional attestation; Oxy excludes such
a route whenever the effective policy has an allow-list or deny-list of regions.

For a concrete model, every accepted entry serves the primary's exact pinned
revision; a cross-model entry is refused. For an exact `routing_profile_id`
target, the first entry is the primary and later entries may cross model lines
only when the contract's literal `authorizedByPolicy: true` is present. Kaana
does not resolve the profile ID or derive candidates from a name: the signed
route list is its entire executable meaning. Same-reference failover emits a
deployment-scoped `route_switch`; a cross-model failover emits a model-scoped
switch naming the primary line, origin and destination.

An absent or empty list grants nothing and is malformed for every supported
envelope version. This applies equally to a concrete target and an exact
routing-profile-ID target: inventory contents, provider declaration order and a
routing-policy reference cannot supply missing authority. During the rollout,
schema v1 is accepted only for a direct-model target that also carries at least
one exact authorized route; its former routing-profile slug arm is refused
rather than resolved.

**A request can move only while nothing has been delivered.** Once output has
reached the customer, retrying anywhere would deliver the beginning of one
answer and the whole of another, so the emitter refuses — and because the
executor asks the emitter rather than keeping its own copy of the rule, there is
one place that knows it. "Delivered" means output: a non-empty delta, a tool
call, audio, or `done`. An adapter's `Start` — which every adapter calls as soon
as the upstream answers 200 — is **held** by the emitter, together with any
usage or empty delta behind it, and written only when the attempt delivers its
first output (or completes with none). A provider that answers 200 and then
reports a failure inside the stream, before any content, has delivered nothing:
its held start is discarded unwritten and the request can still be retried or
moved. The customer sees `start` exactly once, first (after any
`route_switch`), naming the route that actually served; a request that fails
without delivering anything sees only its `error`.

That is also why the `route_switch` event **precedes** the `start` event: the
switch really did happen before anything was streamed, and the contract
specifies event shapes without specifying their order.

## Same-route retry

Failover alone cannot absorb a transient failure on a model with exactly one
authorized route — and many have one (OpenRouter is the only route for
`openai/gpt-6-luna`, which on 2026-09-30 answered five of six high-reasoning
requests with "temporarily rate-limited upstream" inside a 200, while a retry
seconds later usually succeeded). So Kaana retries the SAME route before moving
to the next authorized one, and its callers never have to.

A failed attempt is retried on its own route only when all of these hold
(`kaana.RetryPolicy`, `transientFailure`):

- **nothing was delivered** (the stream has not committed; above);
- **the failure is transient and the deployment's**: `rate_limited`,
  `provider_overloaded`, `provider_timeout`, or `provider_error` with category
  `server_error`. It narrows `provider.DeploymentAttributable`, so nothing the
  breakers do not blame on a deployment is retried there. Never retried on the
  same route: a request fault (`invalid_request`, `model_not_found`,
  `permission_denied`, …), a content filter, a refused credential, a billing,
  quota or all-keys-retired verdict (they do not clear in seconds), a customer's
  own BYOK throttle (`customerlimit` owns that key's backoff), a cancellation,
  anything no adapter classified, and a failure its adapter marked
  `RecursOnThisRoute` (Groq's 413 for a request larger than the account's whole
  per-minute token budget: another route may take it, no wait makes it fit);
- **the route has been retried fewer than `MaxRetriesPerRoute` times** (2); and
- **the wait fits the request's remaining budget** (15 s). The wait is the
  per-route backoff (1 s base, doubling, equal jitter) or the provider's
  `Retry-After`, whichever is longer; a `Retry-After` the budget cannot absorb
  is not waited for — the request moves on, or fails with the provider's hint —
  so the hint caps a wait and never extends the budget. The budget counts
  waiting only: attempt durations are bounded by the attempt cap and by each
  adapter's own stall and timeout bounds (`KAANA_PROVIDER_STREAM_IDLE_TIMEOUT`
  applies per attempt, so a stall retried twice can take three idle windows).

A client that cancels during the wait ends the request at once; the failed
attempt settles as `cancelled` and nothing more is written.

Each retry is a **full attempt**. It goes through the breaker's `Admit` again —
so a half-open breaker whose one trial just failed, or a closed one this
request's failures just opened, refuses it and the request moves to the next
route (announced with `route_switch` there, as usual) — and it reports its own
outcome to its permit, so three consecutive failed attempts open the breaker
whether they came from one request or three. It re-translates and, for BYOK,
re-resolves and re-admits the customer credential. It is its own row in the
operator cost record, with its own attempt index, key, measured units and
outcome; only the terminal attempt can be `served`. A same-route retry is not a
route switch: nothing is announced and `routeSwitches` does not count it.

Key pools: each attempt is a fresh walk over the same exact binding
(`key-pools.md`). A rate limit retires nothing, so a single-key pool reuses its
key — a throttle is not exhaustion.

**A switch is announced at the attempt that replaces the failed one**, not at
the moment of failure — the replacement's own breaker may refuse it, and
announcing early would tell a customer their request moved somewhere it never
went, and put a switch on the receipt that never happened.

**What is never retried — elsewhere or on the same route:** a request the provider could not express (a
refusal about the request, identical everywhere — retrying would make what a
request *means* depend on which route happened to be healthy), a content filter,
a cancellation, and any failure no adapter classified. One function decides,
`provider.AttributableCategory`, and the circuit breakers read the same one, so
the two can never drift apart.

## Circuit breakers and health scoring

There are two state machines in this repository and they are different axes.
This one takes a DEPLOYMENT out when the deployment is failing. Credential
health retires the exact key a deployment binds; production execution does not
escape that binding. Oxy may then attempt the next signed deployment.

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
refusing, timing out, rate limiting, exhausting a quota, or rejecting *Kaana's
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
the provider answers some *other* request than the one it is failing, and Kaana
would be paying for it. A successful trial closes the breaker; a failed one
reopens it with a doubled cooldown, capped, so a long outage is still retried
within a bounded time.

The **health score** is an exponential moving average of attributable outcomes
used in the health projection. It never reorders `authorizedRoutes`: order is a
signed control-plane decision. A deployment nothing has routed to scores 1,
meaning there is no evidence of failure yet.

When every deployment of a model is out of rotation the request is refused with
`deployment_unavailable`, carrying a retry hint that is **the moment the
earliest breaker will admit its next trial** rather than a number chosen to look
reasonable.

## Rules a reviewer applies

- **A `RouteSet` is one exact model reference.** Its endpoints are the only
  same-model candidates for that reference, and an `inventory.Endpoint` never
  names a model. Cross-model execution is possible only when the executor
  resolves a separately signed `authorizedRoutes` entry from another
  `RouteSet`; it must emit a model-scoped `route_switch`. Do not add a model
  reference to `Endpoint`, and do not build a `provider.Route` from inventory
  anywhere but `RouteSet.Candidates()`.
- **Kaana chooses only among the ordered `authorizedRoutes` in the signed
  envelope.** An absent or empty list authorizes nothing and is refused for
  every supported envelope version. Never derive authorization from the
  inventory — it is global and the policy is per customer ("Authorized
  failover" above).
- **A route switch is announced at the attempt that replaces the failed one**,
  never at the moment of failure: the replacement's breaker may refuse it, and a
  switch nobody made must not reach a receipt.
- **A request moves — retried on its route or switched to the next — only while
  nothing has been delivered.** The emitter holds an attempt's `start` until its
  first output and is the one place that knows whether the stream committed; a
  failure after delivery settles as partial and is retried nowhere.
- **A same-route retry is a full attempt.** It passes `Admit` again (never
  bypassing an open or half-open breaker), reports its own outcome, is its own
  cost row, and is never a route switch. Only the transient classes in
  `transientFailure` qualify, bounded by `RetryPolicy`'s attempt cap and wait
  budget.
- **Only `provider.AttributableCategory` decides what a deployment is blamed
  for.** Failover and the circuit breakers read that one function. A customer
  fault, a content filter, a cancellation and an unclassified failure trip
  nothing and are retried nowhere — otherwise one customer's malformed traffic
  takes a healthy route out of rotation for everybody.
- **A deployment returns to rotation on one REAL request through a half-open
  breaker**, one at a time. Never a synthetic probe: it proves the provider
  answers a different request from the one it is failing, and Kaana pays for it.
- **A pinned reference is served from a snapshot of any age; an unpinned one is
  refused past the horizon**, and **staleness is measured from the snapshot's
  own `issuedAt`**, never from when the file was last read. A failed reload
  never disturbs what is being served. The reasoning is in `inventory.md`,
  "Configuration snapshots".

[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0006-oxy-kaana-boundary.md
