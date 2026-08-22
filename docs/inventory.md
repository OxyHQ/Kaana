# The deployment inventory

Which providers serve which model reference, how that snapshot is published, and what happens when publishing stops.

## Configuration snapshots

Kaana's configuration arrives as a file the control plane publishes. If that
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
from here, from a control plane that has stopped publishing, and is treated as
one.

`GET /internal/v1/health` reports the snapshot id, its age, the horizon, whether
unpinned references are still being resolved, and the last reload failure with
no filesystem path in it.

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
| `provider` | `KAANA_PROVIDERS`, filtered to the slugs that hold a credential |
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
| `KAANA_INVENTORY_BUCKET` is empty | refuses to start, naming the variable; there is no default, because publishing to a guessed bucket succeeds silently |
| a cadence at or past the horizon | refuses to start rather than clamping |
| a provider speaking no `GET /models` | refuses; a hand-written list is the checked-in file this command replaces |

### Ordering is load-bearing

Failover is off by default, so a reference resolves to the deployment declared
FIRST and no other. Deployments are emitted in the order `KAANA_PROVIDERS`
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
and are Kaana's. The inventory is the one artefact that has to carry both, so
neither side owns it outright.

It lives here because ADR 0006's "What crosses the boundary" declares exactly
one Oxy→Kaana channel, the per-request envelope, and no channel by which a
catalogue could publish an inventory. The response is to hold the smallest
possible amount of it, declaratively, and never to derive it: an unattributed
model is dropped rather than guessed at. If Oxy grows such a channel, that file
is what it replaces and nothing around it moves.

### Running it

Everything `cmd/kaana` reads about providers, this reads too, from the same
variables through `internal/providerconfig` — so the two commands cannot
disagree about where a provider lives. It uses ONE key from a pool: listing
models is a single unmetered call whose failure means "ask again later", and the
serving process owns the rotation.

| Variable | Required | Meaning |
|---|---|---|
| `KAANA_PROVIDERS` | yes | the slugs to ask; the same list the serving process reads |
| `KAANA_PROVIDER_<SLUG>_API_KEY` | yes, per slug | a slug with no key is dropped with a warning |
| `KAANA_INVENTORY_BUCKET` | yes | the S3 bucket to publish into; never defaulted |
| `KAANA_INVENTORY_KEY` | yes | the object key, e.g. `inventory/current.json` |
| `AWS_REGION` | yes | the bucket's region |
| `KAANA_PUBLISH_INTERVAL` | no | re-issue cadence, default `15m`; refused at or past `KAANA_INVENTORY_MAX_AGE` |
| `KAANA_PUBLISHER_ATTRIBUTION_PATH` | no | default `/etc/relay-publisher/model-attribution.json`, baked into the image |

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



[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0006-oxy-relay-boundary.md
