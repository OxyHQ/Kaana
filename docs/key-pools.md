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

**It is not a route.** A rotation stays inside one deployment: same provider,
same endpoint, same upstream model, same weights, a different credential.
Nothing the customer was told about their route has changed, so no `route_switch`
is emitted and `routeSwitches` stays zero — asserted in the conformance suite,
which fails on a route switch appearing at all.

It therefore needs no additional routing-policy authorization. Choosing among
DEPLOYMENTS is permitted only by entries in the signed `authorizedRoutes` list;
choosing among credentials of one deployment is governed by nothing published,
because the customer-visible route does not change. They are different axes.

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

Keys stated `free` are tried first. Everything else keeps the order it was
declared in — **unstated is not a synonym for paid**, because every deployment
predating this field relies on the declared order, and treating unstated as
paid would silently reorder a live pool the first time somebody classified one
key.

That ordering is the whole mechanism, and it is enough because the walk already
prefers the first usable key and already skips a retired one: a free key that is
out costs one iteration, not a request. It is an ORDER, not a budget. It cannot
cap spend, and a pool whose free keys are all retired spends money — which is
the correct outcome and the reason a budget is a separate thing.

`Key.ID` is the operator's name for a key and is what a log or a health
projection should prefer once a pool holds more than a handful: with twenty
keys, "position 14 retired" names nothing anyone can act on. `Position` keeps
meaning the line the operator wrote, not the slot the key sorted into, so every
error still points at their file.

## Where the credentials come from

PostgreSQL is the only durable store. Each key is one row carrying its provider,
operator id, pool position, declared class, optional budget metadata and KMS
ciphertext. The task environment carries no provider secret, and no manifest or
inventory can carry one.

KMS authenticates `provider + keyId` as encryption context. A database write
that swaps ciphertext between rows therefore produces a decryption failure,
not a credential silently serving under another identity. The configured KMS
key ARN is also checked against every row before decrypting it.

The serving and publisher roles can select active rows and decrypt with that one
KMS key. They cannot insert, update, migrate or encrypt. A short-lived operator
task uses `kaana-credentials put`, which accepts plaintext only on stdin,
encrypts before PostgreSQL sees it and never returns it. Adding a key is a row;
rotating one is an atomic upsert of the same `(provider, keyId)`.

Pools are loaded at process start in `position` order. A declared provider with
no active row is a startup refusal: reporting a green adapter that cannot
authenticate is no longer a supported state. After a credential change, restart
both serving and publisher tasks so their in-memory pools converge on the same
database state.
