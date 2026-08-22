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

### The policy Kaana is not sent, and what it does about it

**Failover is off by default, and that default is a contract finding rather than
caution.** The published `routingFallbackPolicySchema` gives the customer two
booleans that govern exactly this feature — `disabled` and
`sameModelDeployment` — and `routingPolicySchema` adds `allowedRegions` and
`deniedRegions`, which govern where a request may be served at all. The envelope
carries a routing policy **reference** and none of those values. Kaana therefore
cannot tell a customer who asked for failover from one who switched it off, and
failing over anyway would silently override a control the platform advertises.

So with no authorisation, a reference resolves to its **declared primary
deployment and nowhere else** — exactly how this build behaved before failover
existed. Choosing among deployments at all is the policy decision, so health
ordering is withheld too, not just the retry.

`KAANA_ASSUME_FAILOVER_AUTHORIZED=<reason>:<YYYY-MM-DD>` turns it on. It is
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

The **health score** is an exponential moving average of attributable outcomes,
and it orders candidates: admitting breakers first, healthier before flakier,
the inventory's declared order as the tie-break. A deployment nothing has routed
to scores 1 — assuming the worst would sort it permanently last, and it would
never receive the traffic that would prove otherwise.

When every deployment of a model is out of rotation the request is refused with
`deployment_unavailable`, carrying a retry hint that is **the moment the
earliest breaker will admit its next trial** rather than a number chosen to look
reasonable.



[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0006-oxy-relay-boundary.md
