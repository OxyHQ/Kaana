# The wire contract

`@oxyhq/contracts` is the authority; `internal/contract` restates it in Go and is answerable to it.

## The contract is not re-invented here

`@oxyhq/contracts@0.34.0` (contract version 1.3.0) is the wire contract, and the Go types in
`internal/contract` are hand-written against it. Hand-writing is only safe
because two independent gates fail when the two sides diverge.

**1. A generated descriptor, compared field by field.**
`tools/contract/generate.mjs` imports every module under the published package's
`inference/` directory and emits `internal/contract/descriptor.json`: every
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
and a version bump nobody re-derived. Version `0.34.0` adds the ordered
`authorizedRouteSchema`, which Kaana consumes, and model-release/compliance
shapes owned by Oxy, which are recorded not-applicable with exact reasons and an
exact count.

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
published schema itself. It also feeds deliberately invalid fixtures that
the schemas must **reject**, and fails if it saw no fixtures at all: a validator
with a broken schema lookup would otherwise report the same clean run.

Regenerate after a version bump:

```bash
cd tools/contract && bun install --frozen-lockfile && bun run generate
```



[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0006-oxy-kaana-boundary.md
