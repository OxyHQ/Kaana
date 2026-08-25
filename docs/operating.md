# Running and deploying it

Running it locally, the configuration it needs, and what the deployment must supply.

## Running it

Everything comes from the environment and one inventory file. There is no
unauthenticated mode, not even for local development: a bypass that exists is a
bypass that ships.

| Variable | Required | Meaning |
|---|---|---|
| `KAANA_INVENTORY_PATH` | yes | deployment inventory snapshot (see `configs/inventory.example.json`) |
| `KAANA_EDGE_PUBLIC_KEYS` | yes | `kid:base64,…` Ed25519 **public** keys; not secret |
| `KAANA_PROVIDERS` | yes | the provider slugs this process serves, e.g. `openai,openrouter,cerebras,anthropic` |
| `KAANA_PROVIDER_RATES_PATH` | no | upstream rate cards; absent ⇒ provider cost is not measured |

Then, per provider, with `<SLUG>` upper-cased and `.`/`-` replaced by `_`. Two
slugs that collapse onto one variable name are refused at startup rather than
silently sharing an address and a pool.

| Variable | Required | Meaning |
|---|---|---|
| `KAANA_PROVIDER_<SLUG>_PROTOCOL` | for an unknown slug | `openai_compatible` or `anthropic_messages` |
| `KAANA_PROVIDER_<SLUG>_BASE_URL` | for an unknown slug | the provider's API root |
| `KAANA_PROVIDER_<SLUG>_API_KEY` | no | one credential, or a pool separated by commas; absent ⇒ the provider reports `unconfigured` |
| `KAANA_PROVIDER_<SLUG>_HEADERS` | no | `Name=Value` pairs the provider expects, comma-separated (OpenRouter's attribution headers) |
| `KAANA_PROVIDER_<SLUG>_KEY_RETIREMENT` | no | how long a spent or refused key stays out, default `15m` |
| `KAANA_PROVIDER_<SLUG>_KEYS_ON_SEPARATE_ACCOUNTS` | no | `true` when the pool's keys are DIFFERENT provider accounts; only then does a throttle rotate. Anything but `true`/`false` refuses to start rather than quietly meaning "not set" |

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
KAANA_PROVIDERS=cerebras,openrouter,openai
KAANA_PROVIDER_CEREBRAS_API_KEY=…
KAANA_PROVIDER_OPENROUTER_API_KEY=…,…,…          # a pool of three
KAANA_PROVIDER_OPENROUTER_KEYS_ON_SEPARATE_ACCOUNTS=true
KAANA_PROVIDER_OPENROUTER_HEADERS=HTTP-Referer=https://oxy.so,X-Title=Oxy
KAANA_PROVIDER_OPENAI_API_KEY=…
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
| `KAANA_INVENTORY_MAX_AGE` | no | staleness horizon for unpinned resolution, default `1h` |
| `KAANA_INVENTORY_RELOAD_INTERVAL` | no | default `30s` |
| `KAANA_ASSUME_FAILOVER_AUTHORIZED` | no | `<reason>:<YYYY-MM-DD>`; absent ⇒ no failover, see above |
| `KAANA_BREAKER_FAILURES_TO_OPEN` | no | default `3` |
| `KAANA_BREAKER_COOLDOWN` | no | default `5s` |
| `KAANA_BREAKER_MAX_COOLDOWN` | no | default `2m` |
| `KAANA_BREAKER_SUCCESSES_TO_CLOSE` | no | default `1` |
| `KAANA_ADDR` | no | default `:8080` |
| `KAANA_EDGE_MAX_SKEW` | no | default `5m` |
| `KAANA_MAX_ENVELOPE_BYTES` | no | default `16777216` |

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

**Both services, in one release.** The image carries two binaries and there are
two ECS services — `relay` serves and `relay-publisher` writes the inventory the
serving tasks read — so the deploy repoints both, `relay` first. One image built
from one commit is what keeps the publisher from producing snapshots against a
contract the reader has moved past, and that only holds if both are rolled
forward together.

It is also the only thing that delivers a CONFIGURATION change to the publisher.
oxy-infra's `aws_ecs_service.relay_publisher` carries
`ignore_changes = [task_definition]` on the bargain that Terraform owns what a
revision contains and CI owns which revision runs; a Terraform change therefore
registers a revision and does not adopt it. When only `relay` was deployed here,
the publisher's half of that bargain had no holder: an added provider reached
the serving task and never reached the process that discovers models, and the
symptom was a snapshot that quietly went on naming the old provider set.

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

Without any probe, ECS still replaces a task that *exits*, and Kaana exits
non-zero on every startup failure reachable from configuration. What is
uncovered is "running but not serving". The alternative that would close that
gap without a shell is a self-probe flag on the binary, which does not exist
today and is a change to `cmd/kaana`, not to the image.

### What the deployment must supply, and what happens when it does not

**`KAANA_PROVIDERS` is required and is not a secret.** It lists the slugs this
process serves, and an empty one is a hard refusal at startup rather than a
process that serves nothing quietly. It belongs in the task definition's plain
environment beside `KAANA_EDGE_PUBLIC_KEYS`, and its value is **the slugs that
have a key**, not the slugs the platform intends to offer eventually:

```
KAANA_PROVIDERS=cerebras
```

`openai`, `anthropic`, `openrouter` and `cerebras` carry a built-in protocol and
base URL, so a slug from that set needs nothing but its key. Any other slug must
also be given `KAANA_PROVIDER_<SLUG>_{PROTOCOL,BASE_URL}`. The rest of the
per-provider surface — `_HEADERS`, `_KEY_RETIREMENT`,
`_KEYS_ON_SEPARATE_ACCOUNTS` — is non-secret too and goes in the same block.

**Do not declare a slug whose key does not exist yet.** An adapter with no
credential reports `unconfigured` for as long as it is declared, which is
exactly the condition worth alarming on when a key goes missing later — so
declaring ahead of the key pins that alarm on permanently and destroys it. A
signal that is always firing is not a signal.

Three constraints govern the set, and only the last is fatal:

```
KAANA_PROVIDERS   ⊇ the providers the snapshot routes to   else those references are refused
KAANA_PROVIDERS   ⊆ the providers that have a key          else a permanent `unconfigured`
secrets[]         ⊆ the SSM parameters that exist          else the task never starts
```

The first two together are a constraint on whoever publishes the inventory: a
snapshot may not name a provider whose key does not exist, because there is no
value of `KAANA_PROVIDERS` that serves it without either refusing its references
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
policy Kaana is not sent".

**Adding a provider runs in one order and retiring one runs in the reverse**,
both for the same reason — the third constraint above is the only fatal one.
Add: the repository secret and its name in the workflow's allow-list, then run
the workflow so the parameter exists, then the slug in `KAANA_PROVIDERS` and the
parameter's name in `secrets[]`. Retire: out of `secrets[]` and
`KAANA_PROVIDERS`, deploy, and only then delete the parameter. Deleting it while
it is still named stops every task rather than that provider's routes.

**Provider credentials are the only secrets**, one per declared slug. The
deployed set is a subset of these, tracking whichever keys exist:

| SSM parameter | Type | Env var |
|---|---|---|
| `/oxy/relay/KAANA_PROVIDER_CEREBRAS_API_KEY` | `SecureString` | `KAANA_PROVIDER_CEREBRAS_API_KEY` |
| `/oxy/relay/KAANA_PROVIDER_OPENROUTER_API_KEY` | `SecureString` | `KAANA_PROVIDER_OPENROUTER_API_KEY` |
| `/oxy/relay/KAANA_PROVIDER_OPENAI_API_KEY` | `SecureString` | `KAANA_PROVIDER_OPENAI_API_KEY` |

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
goes SSM parameter first, then `secrets[]`, then `KAANA_PROVIDERS`, because each
step is inert until the one before it exists; **retiring** goes the other way —
out of `KAANA_PROVIDERS`, then out of `secrets[]` and rolled out, and only then
delete the parameter, because a `secrets` entry outliving its parameter stops
every task.

**A credential for a slug `KAANA_PROVIDERS` does not declare is inert**, and the
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
`secrets`. Kaana holds only public keys and cannot construct an envelope it
would itself accept; storing a signing key here would destroy that property.

```
KAANA_EDGE_PUBLIC_KEYS=oxy-edge-2026-08-17:jQBxDX3B/Z0ULOHPbQz3gfFinKpl7Qv5MVBTfRYSd34=
```

**`KAANA_ASSUME_FAILOVER_AUTHORIZED` is left unset**, which is the strict
setting: a model reference resolves to its declared primary deployment and
nowhere else. Setting it asserts that every caller of the process has a routing
policy permitting same-model failover, and the envelope carries nothing that
would let Kaana check that. It is not for a shared production deployment, and
`cmd/kaana` refuses to start on a bare `true` precisely so it cannot arrive as
one.

### The configuration snapshot is mounted, not baked

`KAANA_INVENTORY_PATH` defaults to `/etc/relay/inventory.json` in the image, and
the image ships no file there. Baking one in would freeze its `issuedAt`: past
`KAANA_INVENTORY_MAX_AGE` every unpinned reference is refused, so the deploy
would go green and start degrading an hour later. The snapshot has to be
re-issued on a cadence shorter than the horizon, which is a property of a
publisher, not of a file — see "Configuration snapshots".

So `/etc/relay` is a volume, and something publishes into it. **The publisher is
`cmd/kaana-publisher`, in this repository** — see "Publishing the inventory". It
writes one S3 object; what carries that object into the volume is a deployment
choice, and two mechanisms fit ECS Fargate:

- **A sidecar syncing from S3** into a task-scoped volume shared with the relay
  container. Kaana's own reload loop picks the file up within
  `KAANA_INVENTORY_RELOAD_INTERVAL`. The sidecar should download to a temporary
  file and `rename(2)` over the destination: a reader that catches a partial
  write survives it from the second snapshot onward, but on the FIRST there is
  no last-good snapshot to fall back to and the process exits non-zero.
- **An EFS access point** mounted by both the publisher and Kaana.

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

`KAANA_PROVIDER_RATES_PATH` is left unset unless a real rate card is published
the same way. Unset means provider cost is not measured, and every measurement
says so rather than reporting zero.



[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0006-oxy-relay-boundary.md

## Declaring credentials in a manifest instead of the environment

`KAANA_KEYRING_PATH` points at a declaration of which credentials this
deployment holds. Set, it REPLACES the whole per-provider environment block —
`KAANA_PROVIDERS` and six variables each. Unset, nothing changes.

```json
{
  "providers": {
    "openrouter": {
      "keysOnSeparateAccounts": true,
      "headers": { "HTTP-Referer": "https://oxy.so", "X-Title": "Oxy" },
      "keys": [
        { "keyId": "openrouter-free-01", "secretEnv": "KAANA_KEY_OR_FREE_01", "class": "free" },
        { "keyId": "openrouter-oxy-main", "secretEnv": "KAANA_KEY_OR_MAIN", "class": "paid", "budgetUsd": 500 }
      ]
    }
  }
}
```

`configs/keys.example.json` carries the shape and the reasoning.

**No credential is in the file.** A key names the VARIABLE holding its value,
and the value arrives the way every other secret does. A manifest of secret-store
paths would instead make a serving process that cannot start when that store is
unreachable, which is a poor trade for a data plane whose job is availability.

**It is the whole declaration or it is absent.** The two are never blended: a
half-manifest whose gaps were filled from the environment would be a
configuration nobody wrote and nobody could read back.

**Both paths obey one validation.** Protocol, address and header rules live in
`validateProvider` and are called from each. Two copies drift, and the drift is
invisible — the path nobody exercised is the one that accepts what the other
refuses.

**`budgetUsd` is declared and NOT enforced by this build.** The process names
every key that declared one, at startup, as a warning: an operator who declares
a cap and is not told it is inert believes they are protected, which is worse
than having declared nothing. Enforcing it needs accumulated spend in durable
storage, which is what the key id in the operator log exists to make possible.
