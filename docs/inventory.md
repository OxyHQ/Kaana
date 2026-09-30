# The deployment inventory

Which providers serve which model reference, how that snapshot is published, and what happens when publishing stops.

## Configuration snapshots

Kaana's routing configuration arrives as a file its dedicated inventory
publisher writes. If that pipeline stops, the serving data plane must not stop — and must not start
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
file that decays. Past the horizon (`KAANA_INVENTORY_MAX_AGE`, default one hour)
an unpinned reference is refused with `service_unavailable`, retryable, naming
the age and saying that a revision-pinned reference is still served. Guessing
instead would serve weights Oxy may have replaced hours ago on a decision nobody
made.

**Prices do not enter routing** — Kaana holds no customer price, and the one
amount a snapshot may carry, a provider's published list price in a
deployment's `observed` block, is catalogue metadata that never reaches a route
(`cost.md`, "Published list prices"). That removes the hardest half of the usual
stale-configuration problem. The only thing that decays here is a routing
choice, and it degrades rather than breaking.

**One requirement this places on the publisher:** staleness is measured from the
snapshot's own `issuedAt`, not from when Kaana last read the file. That is the
only measure that survives the failure that matters — a publisher that has
stopped running leaves a perfectly readable file on disk, and re-reading it
every thirty seconds would report it fresh forever. So the snapshot must be
**re-issued on a cadence shorter than the horizon even when nothing has
changed**. An unchanged snapshot with an old `issuedAt` is indistinguishable,
from here, from an inventory publisher that has stopped, and is treated as one.

`GET /internal/v1/health` reports the snapshot id, its age, the horizon, whether
unpinned references are still being resolved, and the last reload failure with
no filesystem path in it.

## Signed exact deployment descriptors

`POST /internal/v1/deployments/query` is the operator surface Oxy uses to turn
an opaque Kaana deployment id into the exact route identity it must sign. The
request body is part of the existing Oxy-to-Kaana Ed25519 signature. There are
only three accepted JSON bodies:

List the serving snapshot:

```json
{}
```

Look up one exact identity:

```json
{"deploymentId":"dep_exact"}
```

Attest one bounded set of exact identities from one serving snapshot:

```json
{"deploymentIds":["dep_exact_a","dep_exact_b"]}
```

The first lists the serving snapshot; the second compares the decoded opaque id
for exact equality and returns a list of length one. The batch accepts 1–64
unique exact ids and is atomic: if any requested id is absent, the whole request
is refused without returning the matching subset. `null`, empty or duplicate
batch entries, unknown or extra fields, duplicate keys, malformed or trailing
JSON, an empty body, and URL query parameters are refused. Moving an id from one
value to another therefore invalidates the signature rather than changing an
unsigned selector.

The response contains `snapshotId` and a `deployments` array whose entries have
only `deploymentId`, revision-pinned `modelReference`, `provider`, `regions`
and, when the deployment's provider stated it, `acceptedParameters` — the same
per-deployment set `Candidates()` copies into the route Translate checks
(absent unknown, `[]` a statement). It is not route identity and Oxy's
attestation does not compare it; Oxy stores it so it never signs a route
Translate would refuse, which the catalogue's per-line intersection cannot tell
it when several providers serve a line. It never exposes `upstreamModelId`, endpoints or credential state. `regions: []`
is meaningful: no upstream execution/residency region is attested, and Kaana's
AWS region must not be substituted. Entries are sorted by `deploymentId` only
to make the projection stable for operators; array order is not routing
priority and no lookup selects by position, model name or provider. This
presentation sort is separate from Oxy's request selection: profile priority,
then score descending, then exact ID code units solely as an equal-score
tie-break. The descriptor endpoint supplies identity evidence, not route quality.

Every response sets `Cache-Control: no-store`, including a `401`. A missing or
invalid signature is `401`; an invalid signed body is `400`; an absent exact id
in either lookup shape is `404 no_route_available`; and any duplicate deployment identity is a
fail-closed `503 service_unavailable`. Inventory loading already rejects
duplicates, and the HTTP projection checks uniqueness again independently.

## Publishing the inventory

`cmd/kaana-publisher` is the publisher the section above describes a requirement
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
| `provider` | `KAANA_DISCOVERY_PROVIDERS`, filtered to the slugs that hold a credential |
| `upstreamModelId` | that provider's own `GET /models`, verbatim; for the two xAI profiles also xAI's own answer at the endpoint the model is served on ("xAI speech discovery candidate", "xAI voice discovery" below) |
| `regions` | explicit `KAANA_PROVIDER_<SLUG>_REGIONS`, backed by upstream execution/residency terms; never `AWS_REGION` |
| `modelReference` | `configs/model-attribution.json` for the publisher namespace, plus the observation date |
| `current` | true; each model line has exactly one revision, its observation |
| `deploymentId` | derived from the three above, so an unchanged re-issue keeps its ids |
| `observed` | the same `GET /models` entry, field by field; absent when the provider said nothing Kaana could keep (below) |
| `issuedAt` | the clock, every cycle |
| `snapshotId` | a hash of the routing CONTENT, so it moves only when routing does |
| `withheld` | discovered deployments Kaana's own evidence says cannot be served now; not routes, not hashed ("Withheld from publication" below) |

The last two are deliberately different clocks. An operator asking "is the
publisher alive" reads `issuedAt`; asking "did routing change" reads
`snapshotId`. One value answering both would move every cycle and answer
neither.

### Catalogue metadata is observed, never curated

Each deployment may carry an `observed` block: what its provider's own model
list said about the model when the publisher read it. Nothing is typed in by
hand and nothing is defaulted — an absent field means the provider did not say.

| `observed` field | Read from |
|---|---|
| `displayName` | `name` (OpenRouter, Mistral) |
| `createdAt` | `created`, unix seconds; `0` is absent |
| `contextTokens` | `context_length` (OpenRouter), `context_window` (Groq), `max_context_length` (Mistral) |
| `maxOutputTokens` | `top_provider.max_completion_tokens` (OpenRouter), `max_completion_tokens` (Groq) |
| `inputModalities`, `outputModalities` | `architecture.*_modalities` (OpenRouter), provider words passed through, sorted |
| `supportsTools` | `"tools"` in `supported_parameters` (OpenRouter); `capabilities.function_calling` (Mistral) |
| `reasoningEfforts` | `"reasoning"`/`"reasoning_effort"` in `supported_parameters` → `["low","medium","high"]`, else `[]`; for `xai`, the adapter's own per-model set (`providerconfig.ReasoningEfforts`, `adapters.md`), which is what `Translate` enforces — the one field not read from the list |
| `acceptedParameters` | `supported_parameters`, mapped onto Kaana's request-path vocabulary (below) |
| `listPrice` | OpenRouter `pricing.prompt`/`completion` only, USD per token → per million (`cost.md`) |

A PRESENT `supported_parameters` list is a complete statement, so a parameter it
omits is reported unsupported; an absent list leaves `supportsTools`,
`reasoningEfforts` and `acceptedParameters` absent.

**For OpenRouter the parameter list is not the `/models` entry's.** Every Kaana
request to OpenRouter requires zero data retention and that the serving
endpoint accept every parameter sent, so what a Kaana route accepts is what
the model's zero-data-retention endpoints accept. The publisher reads
OpenRouter's public `GET /api/v1/endpoints/zdr` once per cycle and derives
`supportsTools`, `reasoningEfforts` and `acceptedParameters` from the UNION of
that model's zero-retention endpoints' `supported_parameters`; if any of those
endpoints lists none, the three stay absent. A model with NO zero-retention
endpoint can never be served under that policy and is dropped (the table
below). A failed or malformed read of that list fails OpenRouter's discovery
for the cycle, exactly as a failed `/models` read does
(`provider-onboarding.md`, "OpenRouter").

`acceptedParameters` names caller controls by the contract request path a
caller sets and a refusal names (`provider.RequestParameter`), sorted, from a
closed vocabulary: `maxOutputTokens` (`max_tokens` or `max_completion_tokens`
— one control), `reasoning.effort` (`reasoning` or `reasoning_effort`),
`responseFormat` (`response_format`), `sampling.frequencyPenalty`,
`sampling.presencePenalty`, `sampling.seed`, `sampling.stopSequences`
(`stop`), `sampling.temperature`, `sampling.topP`, `toolChoice`, `tools`.
`sampling.topK` is deliberately absent: no adapter a discovered provider is
served through sends it. The same three states as `reasoningEfforts`: absent is
unknown, `[]` is "reported, takes none of these", a list names them. Only a
provider whose own list states it gets one; every other provider's is absent.

**It is the one observation that reaches a route.** `Candidates()` copies
`acceptedParameters` into `provider.Route`, and `Translate` refuses, with
`invalid_request` naming the field, a control the route's KNOWN set lacks —
before anything is spent, instead of a provider 404 or a silently dropped
parameter. An unknown set refuses nothing. It can only refuse: it never
changes where a request goes or what is sent, so `snapshotId` still does not
hash it. Each field is decoded independently: a field of the
wrong type, zero, negative or unreadable is dropped and never fails discovery,
because a provider's metadata change must make the catalogue say less, not
withdraw its models. `inventory.Parse` validates the block (bounded name,
positive limits, sorted modality tokens, contract efforts, canonical decimal
prices); the publisher's round-trip through it is what keeps a bad block from
being written.

`observed` is presentation metadata. `Candidates()` copies none of it into a
route except `acceptedParameters`, which can only refuse, so it cannot change
what is sent, and `snapshotId` does not hash it: a provider renaming a model
is not a routing change.

`GET /internal/v1/models` aggregates each line's current-revision deployments
into its catalogue entry (`inventory.CatalogueEntry`). A deployment whose
provider said nothing ABSTAINS; one that reported can only narrow:
`displayName` is the first in deployment-id order, `createdAt` the earliest,
`contextTokens`/`maxOutputTokens` the smallest, modalities and
`reasoningEfforts` and `acceptedParameters` the intersection, `supportsTools`
true only if every reporter says so, and `listPrices` one entry per pricing
deployment, never combined.
`reasoningEfforts: []` means a provider reported the model takes no effort;
absent means nobody said. An entry:

```json
{
  "model": "openai/gpt-oss-120b",
  "modelReference": "openai/gpt-oss-120b@observed-2026-08-06",
  "providers": ["groq", "openrouter"],
  "displayName": "OpenAI: gpt-oss-120b",
  "createdAt": "2025-08-05T15:37:04.000Z",
  "contextTokens": 131072,
  "maxOutputTokens": 32768,
  "inputModalities": ["text"],
  "outputModalities": ["text"],
  "supportsTools": true,
  "reasoningEfforts": ["low", "medium", "high"],
  "acceptedParameters": ["maxOutputTokens", "reasoning.effort", "responseFormat",
    "sampling.frequencyPenalty", "sampling.presencePenalty", "sampling.seed",
    "sampling.stopSequences", "sampling.temperature", "sampling.topP",
    "toolChoice", "tools"],
  "listPrices": [
    {"deploymentId": "dep_openrouter_openai_gpt_oss_120b_observed_2026_08_06",
     "provider": "openrouter", "currency": "USD", "input": "0.072", "output": "0.28"}
  ]
}
```

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
| a declared provider holds no active database credential | publisher startup refuses; it must not emit a route the serving process cannot authenticate |
| a provider serves a model nobody attributed | dropped, warned; inferring a publisher from a model id is a claim about somebody else's work |
| an attributed OpenRouter model has no zero-data-retention endpoint | dropped, warned by name; every Kaana request to OpenRouter requires one, so a route to it would fail every request |
| one provider cannot be asked | its routes are absent for that cycle; the others still publish |
| no provider could be asked | the cycle refuses and the published snapshot is left alone |
| the previous snapshot cannot be read | the cycle refuses rather than re-date every reference |
| `KAANA_INVENTORY_BUCKET` is empty | refuses to start, naming the variable; there is no default, because publishing to a guessed bucket succeeds silently |
| a cadence at or past the horizon | refuses to start rather than clamping |
| a provider speaking no `GET /models` | refuses; a hand-written list is the checked-in file this command replaces |
| the exact key a deployment executes on is retired (exhausted or refused) | withheld until the key's return time, named in `withheld`, warned |
| an unbound deployment of a several-key provider whose every key is retired | withheld until the earliest return; with any usable key it stays listed (serving refuses it as unbound, and the operator needs its id to bind it) |
| fresh operator capacity evidence reads zero (balance, a day/month/lifetime quota, a passed expiry) | withheld until the evidence stops being fresh |
| a billing or credential refusal since the deployment's last success on its key | withheld for the quarantine (below) |
| `KAANA_PUBLISHER_WITHHOLD_FAILURES` provider-side failures spanning `KAANA_PUBLISHER_WITHHOLD_FAILURE_SPAN`, no success | withheld for the quarantine (below) |
| rate limits, overloads, timeouts, request faults, however sustained | never withheld |
| every servable deployment is withheld | the cycle refuses and the published snapshot is left alone |
| the publication evidence cannot be read | the previous snapshot's unexpired withholdings are kept; nothing new is withheld; logged at ERROR |

### Withheld from publication

A provider answering `GET /models` proves the credential authenticates, not
that the account can be served. Cerebras lists models to an account that
answers every completion with a 402, and so do the OpenAI and CheaperInference
accounts Kaana held on 2026-09-30: Oxy was offered routes that always failed.
So before a discovered deployment is written, the publisher asks what Kaana
has ALREADY been told about serving it, and withholds it while that evidence
says it cannot be served now (`internal/publisher/withholding.go`). Publication
is Kaana's authority; withholding never reorders anything, and order stays
Oxy's.

Every input is a report Kaana already persisted; the publisher sends nothing
of its own:

1. **The exact key.** A deployment is judged by the one key it executes on, by
   the same rules as `provider.Registry.ResolveExecution`: its exact binding,
   else its provider's only enabled key (`key-pools.md`). That key retired by
   the provider's own exhaustion or refusal withholds it until the key's return
   time, because serving would refuse it until then anyway. A deployment that
   resolves to no key (unbound on a several-key provider) is withheld only when
   every key of its provider is retired: serving refuses it as unbound whatever
   is published, and it stays listed so an operator can find the id to bind.
2. **Capacity evidence** (migration `0017`, recorded by an operator): the key's
   latest FRESH balance at zero, quota at zero over a `day`, `month` or
   `lifetime` window, or a passed expiry, withholds until the evidence stops
   being fresh. A `minute` or `hour` quota at zero is a throttle and never
   withholds. Evidence without `freshUntil` is never fresh.
3. **The deployment's own attempts** (migration `0019`,
   `kaana_read_deployment_failure_streaks`): the failed attempts on its key
   since its last success there — a streak, not a rate. Attempts older than the
   key row's last change (a rotation) are not evidence about the secret now in
   it. Within the streak:
   - one `provider_billing_refused` or `provider_credential_invalid` withholds:
     it is the provider's own report about the key. Capacity evidence recorded
     AFTER that refusal, reading more than zero, lifts it (an operator topped
     the account up);
   - `model_not_found`, `permission_denied` and `provider_error` withhold after
     `KAANA_PUBLISHER_WITHHOLD_FAILURES` of them spanning at least
     `KAANA_PUBLISHER_WITHHOLD_FAILURE_SPAN`. A per-request `permission_denied`
     would be interleaved with successes; an unbroken streak of them on one
     deployment is the account lacking access to the model;
   - `rate_limited`, `provider_overloaded`, `provider_timeout`, request faults,
     content filters, cancellations and any code this build does not name count
     for nothing, however sustained. A throttle is not an outage, and a timeout
     is also what a long reasoning request looks like. They neither count nor
     end the streak; only a success ends it.

**It comes back on a clock, because nothing else can bring it back.** A
withheld deployment receives no traffic, so no success can ever arrive to
restore it. The quarantine after a streak's last counted failure is as long as
the streak has lasted, clamped between `KAANA_PUBLISHER_WITHHOLD_MIN` and
`KAANA_PUBLISHER_WITHHOLD_MAX`: twenty minutes of failures withhold for thirty,
a day of them re-publishes for a trial every six hours, and each failed trial
lengthens the streak and so the next quarantine. The trial is real traffic
through the serving breaker, which admits one request at a time once it opens,
and Oxy's authorized failover absorbs a failure before any output. A success
ends the streak and the next cycle publishes normally. A retired key needs no
clock of its own: its return time is the provider's or `key-pools.md`'s window,
and after it the key is a recovery candidate for the first real request.

**No synthetic probe.** `GET /models` answering is the only request the
publisher sends. An inference probe would spend tokens every cycle on every
deployment and prove only that the provider answers a request other than the
customers' (`routing.md`). A balance endpoint would be free, but none serves
the case this closes: Cerebras documents no balance endpoint for an inference
key (its account metrics sit behind a separate management key), OpenAI exposes
none to a project key, and OpenRouter's `/credits` answered `total_credits: 0`
while a completion on the same key was billed (`key-pools.md`, "Key class").
DeepSeek documents `GET /user/balance`, but it is not verified from this
repository and a probe written from documentation alone is the guess
`key-pools.md` refuses. An operator who reads a balance records it as capacity
evidence, and the rule above reads it.

**Why `snapshotId` moves, and why it does not flap.** Withholding changes the
set of routes, so it moves `snapshotId`: routing changed, and saying otherwise
would let Oxy believe a route exists that serving refuses. What is NOT hashed is
the evidence. The snapshot's top-level `withheld` list carries each withheld
deployment's id, provider, reference, upstream id, `reason`, `until` and
`failures`, outside `contentID`, so re-stating an unchanged decision every
cycle, as its failure count or return time moves, leaves `snapshotId` exactly
where it was. The id moves when a deployment is withheld and when it comes back,
and the quarantine (at least two cycles at the default cadence, growing with the
streak) bounds how often that can happen. The reader ignores `withheld`, and
it carries no key id or credential state; the publisher's WARN line per
withheld deployment names the key.

**The observation date survives withholding.** `ObservationsFrom` reads the
`withheld` list as well as the routes, so a line withheld for a week returns
under the date it was first observed. Reading only the routes would re-date
every reference to it the cycle it returned.

**When the evidence cannot be read**, the cycle keeps the previous snapshot's
withholdings whose `until` has not passed and withholds nothing new. Publishing
everything would flap each withheld route back for one cycle per database blip;
refusing the cycle would age the whole snapshot toward the horizon over a
question about publication.

| Variable | Default | Meaning |
|---|---|---|
| `KAANA_PUBLISHER_WITHHOLDING` | `enforce` | `report` computes and WARNs every decision and withholds nothing: the rollout mode |
| `KAANA_PUBLISHER_WITHHOLD_FAILURES` | `5` | provider-side failures since the last success that withhold a deployment |
| `KAANA_PUBLISHER_WITHHOLD_FAILURE_SPAN` | `10m` | how long those failures must span, first to last, so a burst inside one bad minute never withholds |
| `KAANA_PUBLISHER_WITHHOLD_MIN` | `30m` | shortest quarantine after a streak's last counted failure; two cycles at the default cadence |
| `KAANA_PUBLISHER_WITHHOLD_MAX` | `6h` | longest quarantine: a deployment dead for days is still re-tried four times a day |
| `KAANA_PUBLISHER_WITHHOLD_LOOKBACK` | `24h` | how far back attempts are read; must exceed the maximum quarantine and be at most seven days |

An unparseable value, a zero threshold or quarantine, an inverted clamp or a
lookback inside the maximum quarantine refuses to start.

The operator reads a decision in three places: the snapshot object's
`withheld` list, a WARN per withheld deployment each cycle (`deploymentId`,
`provider`, `upstreamModelId`, `reason`, `until`, `keyId`, `failures`), and the
`withheld` / `wouldWithhold` counts on the per-cycle `inventory snapshot
published` INFO line.

### Inventory order is presentation only

Every inference envelope carries a non-empty signed `authorizedRoutes` list.
Kaana keeps that exact preference order and inventory never adds to it. Provider
declaration order therefore cannot select a route. The publisher emits only
providers holding a credential and sorts deployments by exact opaque id, so
reordering discovery inputs does not change routing content or its snapshot id.

Two providers of one model line produce ONE reference with two endpoints, which
is the failover set. That is why the observation date is keyed by model LINE and
not by provider: keying it per provider would mint two revisions of one line,
which the reader refuses outright as two `current` revisions.

### The half of this that is Oxy's

`configs/model-attribution.json` maps a provider's own model id onto a canonical
`<publisher>/<model>` line. That is model IDENTITY, which ADR 0006 assigns to
**Oxy** — while `upstreamModelId` and `current`, in the same file, are execution
and are Kaana's. The inventory is the one artefact that has to carry both, so
neither side owns it outright.

It lives here because ADR 0006's "What crosses the boundary" declares exactly
one Oxy→Kaana channel, the per-request envelope, and no channel by which a
catalogue could publish an inventory. The response is to hold the smallest
possible amount of it, declaratively, and never to derive it: an unattributed
model is dropped rather than guessed at. If Oxy grows such a channel, that file
is what it replaces and nothing around it moves.

### Running it

Everything `cmd/kaana` reads about non-secret provider configuration, this reads
too through `internal/providerconfig`, so the two commands cannot disagree about
where a provider lives. Both load their pools from the same PostgreSQL/KMS
store. The publisher uses one exact key id because listing models is one
authenticated catalogue question even when the provider paginates its answer;
rotating or selecting the first pool row would silently change authority.
Serving resolves each deployment to its exact key binding; pool order is not an
execution selector.

| Variable | Required | Meaning |
|---|---|---|
| `KAANA_PROVIDERS` | yes | serving superset; the publisher refuses a discovery slug absent here |
| `KAANA_DISCOVERY_PROVIDERS` | yes | the slugs to ask; an ordered discoverable subsequence of serving's `KAANA_PROVIDERS` |
| `KAANA_PROVIDER_<SLUG>_DISCOVERY_KEY_ID` | for every discovery slug | exact enabled PostgreSQL key id used for discovery; absent ids fail closed and never fall back to pool order |
| `DATABASE_URL` | yes | TLS URL for Kaana's encrypted credential database |
| `KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN` | yes | expected symmetric KMS key ARN |
| `KAANA_PROVIDER_<SLUG>_REGIONS` | no | verified upstream execution/residency regions; absence is an unattested empty set, eligible only when Oxy's effective policy has no regional control |
| `KAANA_INVENTORY_BUCKET` | yes | the S3 bucket to publish into; never defaulted |
| `KAANA_INVENTORY_KEY` | yes | the object key, e.g. `inventory/current.json` |
| `AWS_REGION` | yes | the bucket's region |
| `KAANA_PUBLISH_INTERVAL` | no | re-issue cadence, default `15m`; refused at or past `KAANA_INVENTORY_MAX_AGE` |
| `KAANA_PUBLISHER_ATTRIBUTION_PATH` | no | default `/etc/kaana-publisher/model-attribution.json`, baked into the image |
| `KAANA_PUBLISHER_WITHHOLDING`, `KAANA_PUBLISHER_WITHHOLD_*` | no | the withholding policy; defaults and meaning in "Withheld from publication" |

Publisher startup requires both variables and refuses any discovery slug absent
from the serving set. Thus
adding a serving-only provider cannot make discovery fail, and discovery cannot
publish a provider the serving task would reject as unroutable.

Credentials come from the ECS task role — `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI`
or `_FULL_URI`, refreshed before expiry — falling back to
`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_SESSION_TOKEN`. Neither is
defaulted into existence; with neither present the signer refuses and names what
is missing.

The image carries both binaries. A publisher task overrides `entryPoint` to
`/usr/local/bin/kaana-publisher`; forgetting the override starts `kaana`, which
refuses to boot without a snapshot, so the mistake is loud.

`internal/awssig` remains the narrow S3 signer and is checked against AWS's
published `get-vanilla` test vector. The AWS SDK is used only for KMS.

## Rules a reviewer applies

`cmd/kaana-publisher` builds the snapshot and re-issues it. `internal/publisher`
holds the logic; `internal/awssig` is the signer. The sections above carry the
reasoning; these are the lines a reviewer holds a change to.

- **It re-issues on a cadence INSIDE the horizon even when nothing changed.**
  That is `inventory.Store`'s requirement, not a preference: an unchanged
  snapshot with an old `issuedAt` is indistinguishable from a publisher that has
  stopped. A cadence at or past `inventory.DefaultMaxSnapshotAge` is refused,
  never clamped.
- **`issuedAt` and `snapshotId` are different clocks.** `issuedAt` moves every
  cycle; `snapshotId` hashes the routing CONTENT and moves only when routing
  does. One value answering both questions answers neither.
- **The revision label is an OBSERVATION, carried forward from the previously
  published snapshot, forever.** Recomputing it re-points every reference a
  customer pinned, daily, with everything green. A read that FAILED is not a
  first run — refuse the cycle rather than re-date. Only a 404 mints today.
- **The observation is keyed by model LINE, never by provider.** Two providers of
  one line must be one reference with two endpoints; keying per provider mints
  two `current` revisions of one line, which the reader refuses outright.
- **AN UPSTREAM MODEL ID MUST STILL NAME THE SAME MODEL TOMORROW.** A reference
  promises immutable weights, so never declare an id that resolves elsewhere: a
  provider's ROUTER (`openrouter/auto` — "routed to one of dozens of models"), a
  moving alias (`~z-ai/glm-latest` — "always redirects to the latest"), or a
  DELIVERY-MODE variant (`:batch`, `:thinking`), which is not other weights and
  has no slot in `<publisher>/<model>@<revision>`. Each is well-formed, loads
  without complaint, and misbehaves only in front of a customer. Declaring one
  hands the choice of model to the provider behind a reference that claims to
  name it. `internal/inventory/checked_in_test.go` asserts all three over the
  checked-in snapshot; `provider-onboarding.md` states the same gate for a new
  provider.
- **A slug reaches the snapshot only once `KAANA_PROVIDERS` names it with a
  protocol and a base URL.** The publisher refuses a discovery slug absent from
  the serving set. The serving process, by contrast, WARNS about a snapshot
  provider it has no adapter for rather than refusing to start — the two move on
  different clocks (`key-pools.md`, "Rules a reviewer applies").
- **The snapshot is validated by `inventory.Parse` — the real reader — before it
  is written.** A snapshot Kaana would refuse is one that publishes green while
  the data plane serves its last good one.
- **A model nobody attributed is DROPPED and named, never guessed.** Inferring a
  publisher namespace from a model id is a claim about somebody else's work made
  on a substring. `configs/model-attribution.json` is declared, and it is the
  half of the inventory that is Oxy's — hold the smallest possible amount of it.
- **A provider holding no credential is never declared in the snapshot** —
  today that is a publisher startup refusal (the table above) rather than a
  silent drop — and one provider failing never withdraws the others. A cycle in
  which nobody answered refuses and leaves the published snapshot alone.
- **A deployment Kaana cannot serve now is WITHHELD, on evidence Kaana already
  persisted, and never on a throttle.** The exact key retired or out of fresh
  capacity, a credential refusal since the last success, or a sustained streak
  of provider-side failures (`classifyFailure` is closed: a code it does not
  name counts for nothing). `rate_limited`, `provider_overloaded` and
  `provider_timeout` never withhold. A withheld deployment returns on the
  quarantine clock, because it receives no traffic that could restore it; never
  withhold without a return time.
- **The `withheld` list is evidence, not routing: `contentID` never hashes it,
  and `ObservationsFrom` reads it.** Hashing it flaps `snapshotId` every cycle
  the failure count moves; not reading it re-dates a line on its return.
- **The publisher sends no probe of its own.** Its only upstream requests are
  its discovery profile's catalogue questions — the model list, and for
  `xai-realtime` one read-only session open per attributed voice model the
  list omits ("xAI voice discovery"), which never decides withholding.
  Withholding reads persisted reports, and an unreadable read keeps the
  previous unexpired decisions rather than publishing or refusing.
- **A profile that must NAME a model to ask about it asks only about
  attributed ones.** `Provider.AttributedModels` is filled from
  `configs/model-attribution.json` every cycle; no id is ever composed,
  guessed or taken from documentation.
- **Inventory order is presentation, never routing authority.** Emit only
  providers holding a key and sort the resulting deployments by exact opaque
  id for stable snapshots. Never reorder `authorizedRoutes` by health, price or
  inventory preference.
- **`observed` is what the provider's model list said, never curated, never
  defaulted.** Absent is unknown. It never enters `snapshotId`, and an
  unreadable field is dropped rather than failing discovery. Aggregation lets a
  silent deployment abstain and a reporting one only narrow. Only
  `acceptedParameters` reaches a route, and only to let `Translate` refuse.
- **An OpenRouter route's parameters are its ZERO-RETENTION endpoints'.** Kaana
  forces `zdr`, so the `/models` list (all endpoints) overstates what a Kaana
  request can use; derive from `/endpoints/zdr`, and never publish a model
  with no zero-retention endpoint.
- **Never default `KAANA_INVENTORY_BUCKET`.** A plausible default turns a
  variable that never arrived into "published somewhere else, everything green".
- **It runs in its own process under its own task role.** The write decides all
  routing, so the permission never joins the serving role — and `sts:AssumeRole`
  into a narrow role does not help, because the assume permission would sit on
  the shared role.
- **One key from the pool, never a walk.** Listing models is a single unmetered
  question whose failure means "ask again later"; rotation belongs to the
  serving process.
- **`internal/awssig` is checked against AWS's published `get-vanilla` vector**,
  not a second reading of the spec by the same author. It remains the narrow S3
  signer; the AWS SDK is used only at the KMS boundary.

[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0006-oxy-kaana-boundary.md

## xAI speech discovery candidate

xAI's `/models` does not list its speech endpoint. The `xai_models_and_speech`
profile also authenticates to `/tts/voices`; only successful discovery of both
reviewed eve/rex voices adds the technical endpoint identity `tts`, attributed
explicitly to `x-ai/text-to-speech`. It is an endpoint with provider-managed
weights, using the existing first-observation revision semantics; it does not
claim an immutable upstream model selector. An empty or incomplete voice list
adds no speech deployment. A failed voice discovery fails that discovery cycle.
Neither the discovered identity nor attribution grants Oxy routing authority.

Specification: https://docs.x.ai/developers/model-capabilities/audio/text-to-speech
The reviewed 2026-09-13 real `/tts/voices` response contained both identities.
This publisher change is not yet promoted to production.

## xAI voice discovery

`xai-realtime` uses the `xai_models_and_realtime_sessions` profile
(`internal/publisher/xai_realtime.go`). xAI's authenticated `GET /v1/models`
does not list its Voice Agent models, so the account list alone would publish
no voice deployment. Measured with the production `xai-realtime` key on
2026-09-30:

| Question | xAI's answer |
|---|---|
| `GET /v1/models` | text, image and video models only; no `grok-voice-*` |
| `GET /v1/models/grok-voice-think-fast-2.0` | `404` "does not exist or your team does not have access" |
| `GET /v1/realtime/models`, `/v1/realtime/voices` | `403` "Team is not authorized to perform this action" |
| `POST /v1/realtime/client_secrets` | `200` with an ephemeral secret — a mint, a mutation; not used |
| `wss://api.x.ai/v1/realtime?model=grok-voice-think-fast-2.0` | `101`, then unprompted `session.created` with `session.model: "grok-voice-think-fast-2.0"`, `turn_detection: {"type": null}`, then `conversation.created` and a JSON `ping` |
| the same with `?model=grok-voice-bogus-9.9` | `101`, then `session.created` with `session.model: "grok-voice-think-fast-2.0"` — **xAI silently substitutes its default** |
| the same with no `?model=` | identical to the bogus id |

So neither the upgrade nor a `session.created` is evidence; only a
`session.created` whose `session.model` is **exactly** the id asked for is. For
each id attributed under `xai-realtime` that `providerconfig.ClassifyModel`
calls a conversation session model and the account list does not already name,
the profile dials `{base}/realtime?model=<id>` (on the locked root, exactly
`providerconfig.XAIRealtimeSessionURL`) with the discovery key, writes
nothing, reads at most four events within ten seconds, and closes:

| xAI's answer | Result |
|---|---|
| `session.created` naming exactly the id | discovered, as if the list had named it |
| `session.created` naming anything else (the substitution above, the alias, another case) | absent; discovery continues |
| an `error` event | absent; discovery continues |
| handshake refused, a close, a timeout, a binary or non-JSON frame, a `session.created` with no session, four events without one | that provider's discovery fails this cycle |

Failure follows the speech profile: an ANSWER that the model is not served is
absence; NO answer is a discovery that could not be completed, so the provider
is absent from that snapshot with an error logged, and the other providers are
unaffected. For `xai-realtime` the two differ only in the log line, because
the text models on its list are never published under it.

Because `grok-voice-think-fast-2.0` is also xAI's fallback, a `session.created`
naming it cannot tell "served because asked for" from "served as the default".
It does not need to: `session.model` is what xAI says will run the session,
and that is the fact a deployment asserts. The day xAI's default moves to
another model, a still-served 2.0 keeps answering with its own name, and a
retired 2.0 answers with the new default's name — absent, never re-pointed.

Cost of a probe. xAI (https://docs.x.ai/developers/models/speech-to-speech,
read 2026-09-30): "Sessions using the default `server_vad` turn detection are
billed for session duration. Push-to-talk sessions are billed only for audio
sent and received", and `$0.004` per client `conversation.item.create`. The
probe sends no event and no audio, receives no audio, and the session xAI
opened reported `turn_detection: {"type": null}` — push-to-talk, not the
`server_vad` the pricing page calls the default. By xAI's published terms the
probe is therefore unbilled. That is a reading of the terms, not a billing
statement: no per-session usage record was available to confirm it. The upper
bound, if xAI billed the probe's wall clock as a `server_vad` session anyway,
is under one second at $0.08/minute — about $0.0013 per probe, $0.13 per day
at the 15-minute cadence. Each probe also holds one of the team's 10
concurrent sessions for well under a second.

A line absent from one snapshot takes the date of the cycle it returns in, as
for every provider (`ObservationsFrom` reads only the previous snapshot). A
probe failure that straddles a UTC date change therefore costs the voice line
a new deployment id, and with it a new rate card (realtime.md, "Operating
`xai-realtime`"); one that recovers the same day keeps its id.
