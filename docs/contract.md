# The wire contract

`@kaana.ai/contracts` — `packages/contracts` in this repository — is the
authority; `internal/contract` restates it in Go and is answerable to it. The
direction is **Zod → descriptor → Go** (ADR 0002, decision 10).

## First-party authority (ADR 0004)

An Oxy-owned service or product session uses `authority: 'first_party'` in the
signed principal and carries no `inferenceScopes` (envelopes) or `scopes`
(grants). External callers retain their explicit lists. An absent discriminator
means the historical external format. Reader-first rollout and the invariants
are in [ADR 0004](adr/0004-first-party-service-authority.md).

## History

Every contract set, newest first, with the reasoning behind it, is in
[contract-history.md](contract-history.md); the release notes of each published version are in
[`packages/contracts/CHANGELOG.md`](../packages/contracts/CHANGELOG.md). Add the new set to both
when the version moves ([version.ts](../packages/contracts/src/public/version.ts) holds the rule).

## The contract is not re-invented here

`@kaana.ai/contracts` (contract set 4.0.0) is the wire contract, and the Go types in
`internal/contract` are hand-written against it. Hand-writing is only safe
because two independent gates fail when the two sides diverge.

During the coordinated rollout, Kaana accepts request-envelope schema v1 only
when its target is a direct `model`. It never interprets v1's retired
`routing_profile` slug. The current v2 envelope accepts either a direct model
or `{kind: "routing_profile_id", routingProfileId: <exact opaque Oxy id>}`. A
profile ID carries no routing semantics into Kaana: Oxy's signed
`authorizedRoutes` are the complete ordered authority, and Kaana neither looks
up nor normalizes the ID. Both supported versions require that list to be
non-empty; compatibility never authorizes a deployment by inventory position.
Any other envelope version — including the retired scoped (v3) and Private Auto
(v4) envelopes, removed with contract set 7.0.0 — is refused whole before the
body is interpreted. A decisions envelope is refused on this lane: decisions are
served on `https://kaana.ai/v1/decisions` (decisions.md).

**1. A generated descriptor, compared field by field.**
`tools/contract/generate.mjs` imports every module under the workspace
package's `src/public/` and `src/internal/` and emits
`internal/contract/descriptor.json`: every
shape, field, optionality, kind, enum member, literal value and string
constraint. The shape list is **discovered, not listed**, so a shape added
upstream appears on the next regeneration.
`internal/contract/descriptor_test.go` then requires that:

- every published shape is either implemented in Go or recorded in an exact,
  reason-carrying `notApplicable` list (an exact count, so a shape cannot be
  excused by appending a line);
- every implemented shape's Go JSON field set **equals** the contract's — no
  extra field, no missing field;
- optionality matches (optional ⇔ absentable and `omitempty`, required ⇔ neither);
- enum members match exactly, in both directions;
- the regexes Kaana enforces are character-identical to the published ones;
- `ContractVersion` equals the published `INFERENCE_CONTRACT_VERSION`.

CI regenerates the descriptor and fails on any diff, which catches a hand-edit
and a version bump nobody re-derived. The current contract carries the ordered
`authorizedRouteSchema`, its optional exact customer-credential generation,
and required deployment identity on usage emitted by Kaana. Oxy-only catalogue,
billing and provider-connection metadata remains recorded as not applicable
with exact reasons and an exact count.

**What it catches:** a field renamed, added, removed, or flipped between
required and optional; a scalar's type changed; a reference repointed; a version
literal changed; a variant added to a discriminated union; an enum member added
or removed. `drift_test.go` proves this rather than asserting it — it perturbs
the descriptor in each of those eleven ways and requires the comparator to
report every one, after first confirming the unperturbed comparison is clean.

**2. Kaana's own output, parsed by the real Zod schemas.**
Structure is not values. `go test ./internal/contract/...` marshals one fixture
per wire shape using the same Go types the server uses — with **every optional
field populated**, because an optional field that drifted is invisible in a
minimal fixture — and `tools/contract/validate.mjs` parses each with the
contract's own schema. It also feeds deliberately invalid fixtures that
the schemas must **reject**, and fails if it saw no fixtures at all: a validator
with a broken schema lookup would otherwise report the same clean run.

Regenerate after any change to `packages/contracts/src` (it also writes
`packages/contracts/openapi.json`, OpenAPI 3.1 from `z.toJSONSchema`):

```bash
bun install --frozen-lockfile && (cd tools/contract && bun run generate)
```

## Rules a reviewer applies

- **Never edit `internal/contract/descriptor.json` or
  `packages/contracts/openapi.json` by hand.** Regenerate with the command
  above. CI regenerates and fails on any diff.
- **Never add a field to a produced shape that the contract does not have.**
  The descriptor test fails on it, and that failure is correct: Oxy's parse would
  strip it silently, so the field would appear to work here and do nothing there.
- **A published shape Kaana does not exchange goes in `notApplicable` with a
  reason naming the owner**, and `expectedNotApplicableCount` moves in the same
  change. The count is exact so a shape cannot be excused by appending a line.
- **A contract change is Zod first.** Change `packages/contracts/src`,
  regenerate, read the descriptor diff, then make the Go side agree — in that
  order — and bump `INFERENCE_CONTRACT_VERSION` and the package version by the
  rule in `src/public/version.ts`.
- **Nothing a caller reads that names a provider, key or deployment enters the
  package root.** It goes under `/internal`; `publicSurface.test.ts` refuses it.
  A caller naming a provider it wants, in the `provider` preference of the two
  text requests, is input and the one allowed case.
- **Do not decode ordinary inbound envelopes strictly.** Adding an optional field is
  additive under the contract, so `DisallowUnknownFields` would turn every
  additive Oxy change into an outage here. What *is* refused is an unimplemented
  `schemaVersion`, whole, before any field is interpreted
  (`architecture.md`, "The Oxy-facing surface"). Private v3/v4 authority is
  deliberately strict: unknown or duplicate authority members are refused by
  their separately negotiated codecs.
- **A version is never inferred from the presence of a field.**

[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0006-oxy-kaana-boundary.md
