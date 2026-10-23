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

## Contract set 4.4.0: deployment commercial facts and `sameModelFailover` (ADR 0002, amendment 2026-10-15)

MINOR, `@kaana.ai/contracts` 4.4.0. Additive; everything 4.3.0 parses still parses.

- **`deploymentCommercialSchema`** (`/internal`, strict): `availabilityScope`
  (`availabilityScopeSchema`), `commercialPermission`
  (`commercialPermissionSchema`) and an optional `dedicatedCapacity` (absent =
  shared). `customer_byok` if and only if `byok_only`; `public_payg` requires
  `public_resale_approved`, `wholesale_contract` or `open_weight_hosting`. It is
  the optional `commercial` member of every deployment descriptor on the signed
  `POST /internal/v1/deployments/query` and `/internal/v1/models/query`. Go:
  `contract.DeploymentCommercial` (compared field by field by the descriptor
  test, with `Validate()` for the refinements). The source is
  `configs/deployment-commercial.json` (docs/inventory.md, "Commercial facts").
  `availabilityScopeSchema` and `commercialPermissionSchema` are no longer
  `notApplicable`: Go binds their members.
- **`grantStepLimitsSchema.sameModelFailover`**: `z.boolean().default(true)`.
  `false` makes `resolveGrant` authorize ONE deployment (one provider) per
  admitted line in the step, the best-ranked (cheapest funding tier, then the
  grant's line order, then the inventory's); no other provider of that line is
  behind it, so a failure is not retried on one. Later steps are unaffected. Go:
  `GrantStepLimits.SameModelFailover *bool` (nil = true) and
  `SameModelFailoverAllowed()`. Rotation among the keys of the chosen
  deployment's pool (ADR 0001) is not a provider failover and still applies.

## Contract set 4.3.0: the declared catalogue (ADR 0002, amendment 2026-10-15)

MINOR, `@kaana.ai/contracts` 4.3.0. Additive; everything 4.2.0 parses still parses.

- **`declaredModelSchema` / `declaredCatalogueSchema`** (`/internal`): the
  catalogue Kaana declares in `configs/public-catalogue.json` — `modelSchema`
  without `schemaVersion`, `currentRevision` and `availableRevisions` (Kaana
  fills those from its inventory), plus `audience`, and strict. A declared model
  carries its list price per unit and never `servingProviders`,
  `availabilityScope` or `commercialPermission`.
- **`declaredCatalogue`** is a new member of the signed `GET /internal/v1/models`
  response: the whole file, so Oxy rates grants from Kaana's prices. Both
  schemas are `notApplicable` to the Go descriptor test: Go relays the file as
  raw JSON and `tools/contract validate` parses it with the Zod schema.

## Contract set 4.1.0: input transcription inside a realtime session (Alia ADR 0017, phase 1)

MINOR, `@kaana.ai/contracts` 4.1.0. Everything 4.0.0 parses still parses; a
consumer on 4.0.0 refuses what is new.

- **`input_transcription_milliseconds`** joins the closed unit set: the
  milliseconds of a realtime session's input audio that the deployment's own
  transcription model transcribed, where the provider bills that model apart
  from the session model (OpenAI). A second model's reading, so it sits beside
  the session model's `audio_input_tokens` without breaking the partition
  rule; a provider whose session price includes the transcription (xAI) never
  reports it. Provider-reported. `money.ts` states the rule and why the
  alternatives (`audio_input_milliseconds`, or folding it into the tokens)
  would double-charge or misprice.
- **`capabilities.realtime`** gains `inputAudioTranscription` (the flag that a
  model transcribes its input inside a conversation) and `audioFormats`
  (`{input, output}` realtime encodings). Both optional: absent is undeclared.
  A realtime model also publishes `audio` (voices with `gender` where reviewed,
  languages) with empty file formats.
- **`checkRealtimeSessionOpen`** is the realtime counterpart of
  `checkSpeechRequest`: the issues an open has against a model's catalogue
  entry, refused before a grant.
- `realtimeAudioFormatSchema` moved from `realtime.ts` to `audioFormats.ts`
  (same members, same export name from the package root), so the catalogue
  can name it.
- The grant fact `audioInputMilliseconds` documents how Kaana measures an
  upload (`internal/audioprobe`, `docs/openai-audio.md`, "Measuring an
  upload's duration").

The signed-envelope lane (`/internal/v1/realtime`) keeps refusing input
transcription: its consumer settles with `@oxy.so/contracts`, which has no
unit for it (`docs/realtime.md`).

## Contract set 4.0.0: `@kaana.ai/contracts` (ADR 0002, phase 1)

The inference contract moved here from `@oxy.so/contracts` 4.12.0 (set 3.8.0)
and is the first set published as `@kaana.ai/contracts`, version 4.0.0. Two
entry points:

- **The root** is the public API of `https://kaana.ai/v1`: requests and
  responses of every endpoint, the response and speech streams, the public error
  vocabulary (the internal one minus `provider_credential_invalid`,
  `provider_billing_refused` and `deployment_unavailable`), the catalogue with
  prices, power classes and per-model audio capabilities (voices, languages,
  formats, `speed` range, maximum characters), power levels, transcription,
  realtime and generation records. No public shape names a provider, a key or a
  deployment; `publicSurface.test.ts` walks every one to hold that.
- **`@kaana.ai/contracts/internal`** is what Oxy and Kaana exchange: the grant
  and its authorization request, the settlement, the canonical input hash, the
  operational catalogue, customer-credential control, and the signed v1–v4
  envelope with its stream and usage report, which stays the only wire until
  phase 3.

Billing, the ledger (reservations, receipts, refunds), price versions, the
routing policy as a Console resource, entitlements, Inbox and model-release
ingestion stayed in `@oxy.so/contracts`. Every shape that moved parses the same
bytes as before — the regenerated descriptor matches 4.12.0 shape for shape,
the moved Oxy suites pass, and every Go fixture parses — with two deliberate
changes: the envelope admits `audio_speech` with `stream: true` (decision 8; an
adapter still refuses what its provider cannot stream) and the economic
treatment gains `platform_fee_only`.

Zod is held below 4.5: from 4.5 Zod measures string bounds in code points,
while the contract, Go and the frozen cross-language fixtures measure UTF-16
code units (`zodSemantics.test.ts` fails if that changes).

## Funding tiers, contract set 3.8.0 (`@oxy.so/contracts` 4.12.0)

Contract set 3.8.0 adds `inferenceFundingTierSchema` (`free`, `credit`,
`paid`), an authorized route's optional `maxFundingTier` and the usage
report's optional `fundingTier` (Oxy ADR 0038, ADR 0001). It is pinned from
the published `@oxy.so/contracts` 4.12.0 and `descriptor.json` was regenerated
from that installed package with `OXY_CONTRACTS_LOCAL_SOURCE` unset; the only
change from the earlier local-build descriptor was the removed `source`
provenance.

## Decisions foundation on published 4.7.0

This branch consumes Oxy contract set **3.5.0** / envelope 2 from the published
`@oxy.so/contracts` **4.7.0**, pinned exactly in `tools/contract/package.json`
and locked from the public npm registry (`bun.lock` integrity
`sha512-/VS0Evw+YQ78B/JyoLCKHrFs/A5rY3E5+s8CvNP7QMfW4isT/VhPqIv+8lNVkJHzAJzOSrS5HHoOrM/DVx78qA==`).
`descriptor.json` was regenerated, not edited, from that installed package with
`OXY_CONTRACTS_LOCAL_SOURCE` unset, so it carries no `source` field. The only
change from the earlier local-build descriptor was the removed `source`
provenance (and its generated `$comment`); package 4.7.0, contract set 3.5.0
and every shape were already identical. `generate.mjs` still refuses a local
build unless `OXY_CONTRACTS_LOCAL_SOURCE=<owner/repo>@<commit>` records its
origin, and never labels one published. See [decisions.md](decisions.md).
Publishing the contract enables nothing: no production Jev route is enabled.
The historical rollout notes below predate this foundation.

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
- **Nothing that names a provider, key or deployment enters the package
  root.** It goes under `/internal`; `publicSurface.test.ts` refuses it.
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
