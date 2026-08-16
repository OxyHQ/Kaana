# Relay — rules

> Read `README.md` first for what this is and how it fits together. This file
> holds only the rules a reviewer applies. Architecture belongs in `README.md`
> and package comments; history belongs in git.

## The boundary is the design

Relay is the inference **data plane**. Oxy is the single control plane
([ADR 0005], [ADR 0006]).

**Relay owns:** request normalization · provider adapters · routing *execution*
· streaming · cancellation · model deployments · provider health and circuit
breakers · technical metering · upstream provider cost.

**Relay must never own:** accounts · organizations · projects · members ·
applications · credentials · customer balances · a billing ledger · a customer
console.

- **An Oxy id is an immutable opaque string.** Relay never parses it, joins it
  against a local entity, or updates it. A table whose *primary* key is an Oxy id
  is a copy of an Oxy entity and is forbidden. A column holding one, written once
  at request time and never updated, is the intended shape.
- **Relay authorizes nothing about a customer.** Scope checks, account access,
  credential status and spend reservation are resolved at the Oxy edge; the
  envelope is an already-authorized instruction. Re-deriving any of it
  reintroduces the replication lag that makes revocation unsafe. The one
  exception is refusing an envelope that carries no `inference:invoke` — that is
  a malformed instruction from the edge, not a customer decision.
- **Relay measures units and never prices them.** There is no money type in
  `internal/contract`, and adding one is the moment a second ledger starts.
- **If a change would put an Oxy-owned concept here, the change is wrong, not
  the boundary.** A schema review is a boundary review.

## The contract is not authored here

`@oxyhq/contracts` is the wire contract. `internal/contract` restates it in Go
and is answerable to it.

- **Never edit `internal/contract/descriptor.json` by hand.** Regenerate:
  `cd tools/contract && npm ci && npm run generate`. CI regenerates and fails on
  any diff.
- **Never add a field to a produced shape that the contract does not have.**
  The descriptor test fails on it, and that failure is correct: Oxy's parse would
  strip it silently, so the field would appear to work here and do nothing there.
- **A published shape Relay does not exchange goes in `notApplicable` with a
  reason naming the owner**, and `expectedNotApplicableCount` moves in the same
  change. The count is exact so a shape cannot be excused by appending a line.
- **Bumping the pinned contracts version is its own change.** Regenerate the
  descriptor, read the diff, then make the Go side agree — in that order.
- **Do not decode inbound envelopes strictly.** Adding an optional field is
  additive under the contract, so `DisallowUnknownFields` would turn every
  additive Oxy change into an outage here. What *is* refused is an unimplemented
  `schemaVersion`, whole, before any field is interpreted.
- **A version is never inferred from the presence of a field.**

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
- **An adapter classifies its own failures.** Never infer retryability from an
  HTTP status: a 429 from an exhausted daily quota and a 429 from a burst limit
  are the same status and opposite answers.
- **`Stream` returns the units it measured even when it fails.** A partial
  stream is a settlement case; an adapter that returns nothing on cancellation
  makes an exact refund impossible.
- **`ctx` reaches the upstream HTTP request.** That is the entire cancellation
  design, and it is why an adapter cannot decline to honour it.
- **Never invent a default the caller did not send.** An absent sampling
  parameter means the route's own default.

## Secrets and customer data

- **No provider credential, endpoint secret or Oxy secret in this repository,
  in a test, or in CI.** Upstream credentials come from the process environment.
  The Oxy edge's key here is a *public* key and is ordinary configuration.
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
[ADR 0006]: https://github.com/OxyHQ/sdk/blob/main/docs/adr/0006-oxy-relay-boundary.md
