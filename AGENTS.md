# Kaana

Kaana is Oxy's inference **data plane** (Go); Oxy is the single control plane.
Kaana executes signed, already-authorized requests and never owns accounts,
credentials, a ledger or a console. Alia is the agent runtime, not this repo:
`app -> Oxy edge -> Kaana` or `app -> Alia -> Oxy edge -> Kaana`.

## Commands (CI)

`make check` runs every gate below, in CI's order, from the same pinned
versions. Prefer it: the commands are the ones CI runs, not near-equivalents.
`make help` lists the individual targets and `make tools` reports what is
missing locally.

```bash
make check                      # gofmt, build, vet, lint, test, contract, cloudflare
make test-integration           # needs KAANA_POSTGRES_TEST_URL; refuses to skip
```

The gates it encapsulates, if you are running them by hand:

```bash
gofmt -l .                      # prints nothing
go build ./... && go vet ./...
golangci-lint run               # v2.13.2
go test -race -count=1 ./...
bun install --frozen-lockfile && bunx biome ci .
(cd packages/contracts && bun run typecheck && bun run test && bun run build)
(cd tools/contract && bun run generate && bun run validate)
```

npm releases: merging a version bump to main publishes it (`release-packages.yml`,
which calls Oxy's shared `npm-release.yml` once CI passed on that commit; a
package's gates are CI). Never `npm publish` from a laptop — ~/Oxy/docs/npm-releases.md.

Biome (root `biome.json`) covers ONLY `packages/**` and `tools/**` (ADR 0002).
Never widen it further: `configs/**`, `.github/*.json`, the descriptor and
`packages/contracts/openapi.json` are byte-exact inputs or outputs.

## Rules

Boundary — docs/architecture.md#rules-a-reviewer-applies
- An Oxy id is opaque: never parsed, joined or a primary key.

Contract — docs/contract.md#rules-a-reviewer-applies
- `@kaana.ai/contracts` (`packages/contracts`, Zod) is the source: Zod →
  descriptor → Go. Public `/v1` shapes at the root, Oxy↔Kaana under `/internal`.
- Never edit `internal/contract/descriptor.json` or `openapi.json` by hand; regenerate them.
- The model catalogue (identity, licence, data policy, capabilities, list prices) is
  `configs/public-catalogue.json`, Kaana's alone; no field without primary evidence — docs/operating.md#the-declared-catalogue
- Never add a field the contract lacks; unexchanged shapes go in `notApplicable`.

Routing — docs/routing.md#rules-a-reviewer-applies
- Attempt only the signed `authorizedRoutes`, in order; inventory authorizes none.
- A `RouteSet` is one model reference; build `provider.Route` only in `Candidates()`.
- Only `provider.AttributableCategory` trips a breaker; half-open admits one request.
- A request moves (same-route retry, then failover) only before output is delivered;
  a retry is a full attempt through `Admit`, bounded by `RetryPolicy`.

Inventory — docs/inventory.md#rules-a-reviewer-applies
- Re-issue inside the horizon; staleness is the snapshot's own `issuedAt`.
- The revision label is carried forward per model line, never re-dated.
- An upstream id must name the same weights tomorrow: no routers, aliases, `:batch`.
- Validate with `inventory.Parse` before writing; never default the bucket.
- Withhold what cannot be served now, on persisted evidence, with a return time;
  never on a throttle, never by probing, never hashed into `snapshotId`.

Credentials — docs/key-pools.md#rules-a-reviewer-applies
- PostgreSQL KMS ciphertext is the only key store; plaintext only on stdin.
- A deployment runs on its provider's key pool, free → credits → paid → unstated; a pin is its only key — docs/adr/0001-kaana-owns-provider-keys-costs-and-routing.md
- Retire on reported exhaustion or rejection and move to the next key; never past a pin or a funding ceiling.
- Verdicts come from the adapter's code, never a status; a 402 retires the key.
- BYOK: same custody, inverse authorities, one exact operation id —
  docs/customer-provider-credentials.md#rules-a-reviewer-applies

Cost — docs/cost.md#rules-a-reviewer-applies
- Only `internal/providercost` holds an amount; none enters an inference response.
- A provider's PUBLISHED list price reaches Oxy only on the signed catalogue.

Adapters — docs/adapters.md#rules-a-reviewer-applies
- One `provider.Adapter` and one real-wire fake per provider; pass conformance.
- Refuse in `Translate` what the provider cannot express; invent no default.
- A reasoning effort goes in the provider's own documented field or is refused.
- Classify by the provider's error type; redact your own key by exact match.
- `Stream` returns measured units even on failure; `ctx` reaches upstream.
- A realtime session is a `provider.RealtimeAdapter`, never a branch of a
  request adapter; it settles exactly once — docs/realtime.md#rules-a-reviewer-applies

Cross-cutting — docs/rules.md
- No secret in repo, test, CI or env; none in a log, error or usage record.
- No prompts, completions or user IPs persisted or logged.
- Every gate needs a positive control; mutation-test what is load-bearing.
- No `TODO`/shims; Conventional Commits; wrap errors for `errors.Is`.

## Before touching X, read `docs/`

schema→architecture · envelope→contract · executor→routing · publisher→inventory ·
decisions→adr · keys→key-pools, customer-provider-credentials · provider→adapters,
provider-onboarding · realtime sessions→realtime · deploy→operating.
