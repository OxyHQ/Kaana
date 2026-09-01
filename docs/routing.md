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

## Authorized failover

Oxy sends `authorizedRoutes` in preference order after applying the customer's
routing policy. Kaana attempts that list in exactly that order and never adds an
inventory route, reorders by health, or interprets the policy reference. Before
execution it resolves every `deploymentId` against one inventory snapshot and
requires the signed provider, pinned model reference and region set to agree
exactly. Empty equals empty and means no regional attestation; Oxy excludes such
a route whenever the effective policy has an allow-list or deny-list of regions.

For a concrete model, every accepted entry serves the primary's exact pinned
revision; a cross-model entry is refused. For a routing profile, the first entry
is the primary and later entries may cross model lines only when the contract's
literal `authorizedByPolicy: true` is present. Same-reference failover emits a
deployment-scoped `route_switch`; a cross-model failover emits a model-scoped
switch naming the primary line, origin and destination.

An absent list grants nothing. A concrete target resolves to the inventory's
declared primary and nowhere else, preserving compatibility with envelopes from
before the optional field existed. A routing-profile target names no concrete
destination and is therefore refused without a list. An empty list is malformed.

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



[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0006-oxy-kaana-boundary.md
