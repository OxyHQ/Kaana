# Kaana — rules

> Read `README.md` first for what this is and how it fits together, then `docs/`
> for the part you are touching. This file holds only the rules a reviewer
> applies. Architecture belongs in `docs/` and package comments; history belongs
> in git.

## The boundary is the design

Kaana is the inference **data plane**. Oxy is the single control plane
([ADR 0005], [ADR 0006]).

**Kaana owns:** request normalization · provider adapters · routing *execution*
· streaming · cancellation · model deployments · provider health and circuit
breakers · technical metering · upstream provider cost.

**Kaana must never own:** accounts · organizations · projects · members ·
applications · credentials · customer balances · a billing ledger · a customer
console.

- **An Oxy id is an immutable opaque string.** Kaana never parses it, joins it
  against a local entity, or updates it. A table whose *primary* key is an Oxy id
  is a copy of an Oxy entity and is forbidden. A column holding one, written once
  at request time and never updated, is the intended shape.
- **Kaana authorizes nothing about a customer.** Scope checks, account access,
  credential status and spend reservation are resolved at the Oxy edge; the
  envelope is an already-authorized instruction. Re-deriving any of it
  reintroduces the replication lag that makes revocation unsafe. The one
  exception is refusing an envelope that carries no `inference:invoke` — that is
  a malformed instruction from the edge, not a customer decision.
- **Kaana measures units and never prices them.** There is no money type in
  `internal/contract`, and adding one is the moment a second ledger starts.
- **If a change would put an Oxy-owned concept here, the change is wrong, not
  the boundary.** A schema review is a boundary review.

## The contract is not authored here

`@oxyhq/contracts` is the wire contract. `internal/contract` restates it in Go
and is answerable to it.

- **Never edit `internal/contract/descriptor.json` by hand.** Regenerate:
  `cd tools/contract && bun install --frozen-lockfile && bun run generate`. CI regenerates and fails on
  any diff.
- **Never add a field to a produced shape that the contract does not have.**
  The descriptor test fails on it, and that failure is correct: Oxy's parse would
  strip it silently, so the field would appear to work here and do nothing there.
- **A published shape Kaana does not exchange goes in `notApplicable` with a
  reason naming the owner**, and `expectedNotApplicableCount` moves in the same
  change. The count is exact so a shape cannot be excused by appending a line.
- **Bumping the pinned contracts version is its own change.** Regenerate the
  descriptor, read the diff, then make the Go side agree — in that order.
- **Do not decode inbound envelopes strictly.** Adding an optional field is
  additive under the contract, so `DisallowUnknownFields` would turn every
  additive Oxy change into an outage here. What *is* refused is an unimplemented
  `schemaVersion`, whole, before any field is interpreted.
- **A version is never inferred from the presence of a field.**

## Routing, failover and health

- **A `RouteSet` is one exact model reference.** Its endpoints are the only
  same-model candidates for that reference, and an `inventory.Endpoint` never
  names a model. Cross-model execution is possible only when the executor
  resolves a separately signed `authorizedRoutes` entry from another
  `RouteSet`; it must emit a model-scoped `route_switch`. Do not add a model
  reference to `Endpoint`, and do not build a `provider.Route` from inventory
  anywhere but `RouteSet.Candidates()`.
- **Kaana chooses only among the ordered `authorizedRoutes` in the signed
  envelope.** An absent list means the declared primary and nothing else; a
  routing-profile target without a list is refused. Never derive authorization
  from the inventory — it is global and the policy is per customer.
- **A route switch is announced at the attempt that replaces the failed one**,
  never at the moment of failure: the replacement's breaker may refuse it, and a
  switch nobody made must not reach a receipt.
- **Only `provider.AttributableCategory` decides what a deployment is blamed
  for.** Failover and the circuit breakers read that one function. A customer
  fault, a content filter, a cancellation and an unclassified failure trip
  nothing and are retried nowhere — otherwise one customer's malformed traffic
  takes a healthy route out of rotation for everybody.
- **A deployment returns to rotation on one REAL request through a half-open
  breaker**, one at a time. Never a synthetic probe: it proves the provider
  answers a different request from the one it is failing, and Kaana pays for it.
- **A pinned reference is served from a snapshot of any age; an unpinned one is
  refused past the horizon.** The mapping from immutable weights to a provider's
  model id cannot go stale; which revision is *current* is Oxy's decision and
  does. A failed reload never disturbs what is being served.
- **Staleness is measured from the snapshot's own `issuedAt`**, never from when
  the file was last read — a publisher that has stopped leaves a readable file
  behind, and re-reading it would report it fresh forever.

## The inventory publisher

`cmd/kaana-publisher` builds the snapshot and re-issues it. `internal/publisher`
holds the logic; `internal/awssig` is the signer.

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
  checked-in snapshot.
- **Declaring a provider is a BOOT requirement.** The server refuses to start if
  a routed provider has no adapter configured, so a slug reaches the snapshot
  only once `KAANA_PROVIDERS` names it with a protocol and a base URL.
- **The snapshot is validated by `inventory.Parse` — the real reader — before it
  is written.** A snapshot Kaana would refuse is one that publishes green while
  the data plane serves its last good one.
- **A model nobody attributed is DROPPED and named, never guessed.** Inferring a
  publisher namespace from a model id is a claim about somebody else's work made
  on a substring. `configs/model-attribution.json` is declared, and it is the
  half of the inventory that is Oxy's — hold the smallest possible amount of it.
- **A provider with no credential is dropped, not declared**, and one provider
  failing never withdraws the others. A cycle in which nobody answered refuses
  and leaves the published snapshot alone.
- **Inventory order defines the no-list primary; envelope order defines an
  authorized request.** Emit inventory endpoints in `KAANA_PROVIDERS` order and
  only for providers holding a key. Never reorder `authorizedRoutes` by health,
  price or inventory preference.
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

## Provider credentials

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
- **Class is stated, never inferred.** Measured 2026-08-23: no provider this
  build serves publishes remaining credit — Groq and xAI publish burst limits
  whose counts refill, OpenRouter publishes none and its account endpoint
  answered zero while a completion on the same key was billed.
- **Unstated is not paid.** An unclassified pool keeps the order it was declared
  in, so classifying one key moves that key and disturbs no other.
- **A 402 is the platform's account refusing to be billed**, and it must retire
  the key. It reached the default branch once and became `invalid_request`,
  which told the customer their request was at fault and kept spending an
  account that cannot pay.

## Provider cost

- **`internal/providercost` is the only package that may hold an amount**, it is
  never the contract's money type, and `internal/contract` must not be able to
  reach it (asserted, not reviewed).
- **A cost never enters a stream event, a usage report, an error body or a
  response of any kind.** It is an operator number; the customer's amount is
  Oxy's and always was.
- **An unknown cost is never a zero cost.** A deployment with no rate card, or a
  measured unit nobody priced, says so and names what it could not price.
- **A failed failover attempt is off the customer's receipt and on Kaana's
  cost.** Do not merge the two: the customer never received that output and the
  provider will invoice for it regardless.

## Adapters

- **One implementation of `provider.Adapter` and one fake upstream per
  provider.** If a change to add a provider touches the executor, the stream
  framing or the receipt shape, the abstraction is wrong — fix that instead.
- **Every adapter passes `internal/provider/conformance` before it is
  registered.** The fake upstream must speak the provider's **real** wire format;
  a fake speaking the normalized contract tests nothing, because translation is
  the half with no schema to check it.
- **Adapters never** allocate ids, assign sequence numbers, decide terminality,
  emit `done`/`error`/`route_switch`, resolve a model reference to an upstream
  model id, or apply routing policy.
- **Refuse in `Translate` what the provider cannot express**, with a
  non-retryable code and the field named. Silently dropping a parameter changes
  what the model does while reporting success.
- **An adapter classifies its own failures, and stops there.** What that
  classification means for the KEY is `provider.Walk`'s, once, for every adapter
  — an adapter that reimplemented the rotation rules would be free to
  reimplement them differently. Adapters supply `Send`, `Refuse` and
  `TransportFailure`; `Refuse` closes the response body it read.
- **An adapter classifies its own failures**, from the provider's own error TYPE
  first. Never infer retryability from an HTTP status: a 429 from an exhausted
  daily quota and a 429 from a burst limit are the same status and opposite
  answers — and a failure arriving mid-stream, after a 200, has no status at all.
  Every streaming protocol can fail that way, and an adapter that reads the frame
  it cannot use and stops reports a truncated answer as a completed one.
- **The contract's usage units PARTITION a request, and the normalising
  arithmetic is NOT portable between adapters.** An OpenAI-compatible
  `prompt_tokens` includes its cached tokens; Anthropic's `input_tokens` excludes
  them and its `output_tokens` includes reasoning. Copying one adapter's
  subtraction into another mis-bills silently, because a nested report and a
  disjoint one are the same non-negative integers. State the PHYSICAL request in
  the conformance subject and let the suite do the arithmetic.
- **An adapter redacts its OWN credential by exact match; the contract's pattern
  is a last-resort REFUSAL and never the control.** `provider.RedactSecret`
  removes the value the adapter is holding — the only thing that works on a
  credential with no marker and no issued-token prefix, which the published
  pattern states it cannot see. Relying on the refusal instead loses the whole
  diagnostic, which the conformance suite fails you for.
- **Never redact by replacing the span a credential pattern matched.** The span
  is the MARKER and the secret is what follows it, so a span redaction converts
  "this string is dangerous" into "this string is fine" with the key still in
  it. `contract.SafeErrorText` withholds the whole message or none of it.
- **A parameter the provider REQUIRES and the contract makes optional is
  refused, never supplied.** Choosing it at the adapter, or per deployment,
  changes what the model does while reporting success.
- **`Stream` returns the units it measured even when it fails.** A partial
  stream is a settlement case; an adapter that returns nothing on cancellation
  makes an exact refund impossible.
- **`ctx` reaches the upstream HTTP request.** That is the entire cancellation
  design, and it is why an adapter cannot decline to honour it.
- **Never invent a default the caller did not send.** An absent sampling
  parameter means the route's own default.

## Provider credentials and key pools

A credential is a POOL per provider, and the pool is a different rotation from
the deployment breaker. `internal/provider/credential.go` holds all of it.

- **A key leaves rotation only when something REPORTED that it has nothing
  left** — the provider refusing with its own exhaustion error, or a header that
  provider's declared mapping says means remaining credits, reading zero.
  `unknown` is not `exhausted`, `unavailable` is not `exhausted`, and every
  failure nobody classified leaves the key exactly as it was. Getting this
  backwards disables working credentials on ambiguous signals, which is worse
  than the problem a pool solves.
- **An exhausted key rotates the request to the next one; a REFUSED key does
  not.** Exhaustion is expected and the next key is another account. A refused
  credential is a configuration fault or a provider-side auth failure, and under
  the second every remaining key is refused identically — so walking multiplies
  one failure into a call per key and retires the whole pool on a blip. Both
  directions are conformance checks, and they are a matched pair.
- **A request the PROVIDER refused is retried on nothing.** The next credential
  would be refused identically.
- **The verdict is read from the code the ADAPTER chose, never from a status.**
  `CredentialVerdictFor` is the one function, as `AttributableCategory` is for
  the deployment; the two answer different questions and disagree on purpose.
- **Key rotation is not a route switch** — same deployment, no `route_switch`,
  and no additional routing-policy authorization.
- **A refused credential is not failed over onto the same provider slug.** One
  slug is one adapter and one pool, so "another deployment holds a different
  credential" is true across slugs and false within one; failing over there
  reproduces the pool walk one deployment at a time and burns a key per
  deployment on one blip.
- **A rotation happens only before the response body is read**, so a failure
  arriving mid-stream rotates nothing: the request is committed to the key that
  opened the stream.
- **A retirement is a flat window, never permanent and never a backoff.** A
  quota resets on a cycle and the provider's own reset time wins over the
  window; a process that has permanently disabled every credential it holds is
  the worse failure.
- **A quota header mapping is per provider, lives in the ADAPTER package, and
  maps a header to a MEANING** — never a generic name, since
  `x-ratelimit-remaining` is a burst limit at most providers. The shipped
  mapping is empty under an exact-count assertion; an entry needs a verified
  source.
- **A key's identity outside `internal/provider` is its 1-based POSITION** —
  not the secret and not a hash of it, since a fingerprint confirms a guess.
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

## Secrets and customer data

- **No provider credential, endpoint secret or Oxy secret in this repository,
  in a test, CI, environment or task definition.** Upstream credentials come
  only from PostgreSQL ciphertext decrypted through KMS. The Oxy edge's key
  here is a *public* key and is ordinary configuration.
- **A credential never enters a `Call`, an error, a log, a health projection or
  a usage record.** Authentication is applied at send time and nowhere else.
- **Redact upstream error text before emitting it.** Provider errors routinely
  echo the request that caused them; the contract *rejects* an error body whose
  text looks like a credential, so an unredacted one loses the customer their
  diagnostic entirely. The conformance suite covers this with a control proving
  the upstream really echoed one.
- **Prompts and completions are not persisted and never enter a log line.** Log
  ids, a route, an outcome and a duration.
- **Never persist a user IP** — raw, hashed or geo-derived. The contract's
  `client` metadata block is `.strict()` for exactly this reason; do not widen it.

## Tests

- **A gate needs a positive control.** Ask what the check would report if the
  thing it measures were absent. If that is the same answer, it measures nothing.
  `drift_test.go` and the invalid fixtures in `validate.mjs` are the pattern.
- **A cancellation or propagation test needs an uninterrupted control run.**
  "The upstream saw its caller go away" is also what a request that finished
  reports.
- **Mutation-test a load-bearing assertion**: apply the mutation, confirm it
  applied (a mutation that never applied is indistinguishable from one that
  survived), watch the test fail, restore, watch it pass.
- **Populate optional fields in a fixture.** A field that drifted is invisible
  in a minimal one.
- **Exact counts, not floors**, for exemption lists and fixture sets: a floor
  erodes one defensible line at a time.

## Working here

- Go 1.24. `gofmt`, `go vet`, `golangci-lint run ./...` and `go test -race ./...`
  all gate the PR — run the commands CI runs, not near-equivalents.
- No `TODO`, `FIXME`, `HACK`, no back-compat shims, no deprecated aliases.
  Breaking changes are clean cuts.
- Conventional Commits.
- Errors are wrapped and matched with `errors.Is`/`errors.As`; the executor's
  control flow depends on it.
- Say what is *not* implemented rather than stubbing it. The README's
  out-of-scope list is part of the deliverable.

[ADR 0005]: https://github.com/OxyHQ/sdk/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[ADR 0006]: https://github.com/OxyHQ/sdk/blob/main/docs/adr/0006-oxy-kaana-boundary.md
