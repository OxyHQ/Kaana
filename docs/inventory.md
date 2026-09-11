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

**Prices do not enter this** — Kaana holds none — which removes the hardest half
of the usual stale-configuration problem. The only thing that decays here is a
routing choice, and it degrades rather than breaking.

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
only `deploymentId`, revision-pinned `modelReference`, `provider`, and `regions`.
It never exposes `upstreamModelId`, endpoints or credential state. `regions: []`
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
| `upstreamModelId` | that provider's own `GET /models`, verbatim |
| `regions` | explicit `KAANA_PROVIDER_<SLUG>_REGIONS`, backed by upstream execution/residency terms; never `AWS_REGION` |
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
| a declared provider holds no active database credential | publisher startup refuses; it must not emit a route the serving process cannot authenticate |
| a provider serves a model nobody attributed | dropped, warned; inferring a publisher from a model id is a claim about somebody else's work |
| one provider cannot be asked | its routes are absent for that cycle; the others still publish |
| no provider could be asked | the cycle refuses and the published snapshot is left alone |
| the previous snapshot cannot be read | the cycle refuses rather than re-date every reference |
| `KAANA_INVENTORY_BUCKET` is empty | refuses to start, naming the variable; there is no default, because publishing to a guessed bucket succeeds silently |
| a cadence at or past the horizon | refuses to start rather than clamping |
| a provider speaking no `GET /models` | refuses; a hand-written list is the checked-in file this command replaces |

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
- **Inventory order is presentation, never routing authority.** Emit only
  providers holding a key and sort the resulting deployments by exact opaque
  id for stable snapshots. Never reorder `authorizedRoutes` by health, price or
  inventory preference.
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
