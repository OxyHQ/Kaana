# Kaana

Kaana is Oxy's inference data plane (Go): it executes signed, already-authorized
requests. Oxy is the control plane and owns accounts, ledger and console; Alia is
the agent runtime and lives in another repo.

## Commands

`make help` lists every target and `make tools` reports missing local tools.

```bash
make check             # every CI gate in CI's order, from the same pinned versions
make test-integration  # needs KAANA_POSTGRES_TEST_URL; refuses to skip
go test -race -count=1 ./...
bun install --frozen-lockfile && bun run lint    # Biome (packages/ and tools/ only)
bun run lint:fix                                 # apply Biome fixes
```

## Map

- Start at [docs/inference-rules.md](docs/inference-rules.md): the Oxy-wide inference
  rules (names, the one canonical origin, who may call a provider, key custody).
  Other repos link to it.
- [docs/rules.md](docs/rules.md): secrets, customer data, tests and working rules.
- Each topic doc ends with "Rules a reviewer applies"; read it before touching that area.
  - schema, boundary: [architecture](docs/architecture.md)
  - envelope, descriptor, catalogue: [contract](docs/contract.md), [operating](docs/operating.md)
  - executor, breakers, retries: [routing](docs/routing.md)
  - publisher, withholding: [inventory](docs/inventory.md)
  - provider keys, BYOK: [key-pools](docs/key-pools.md), [customer-provider-credentials](docs/customer-provider-credentials.md)
  - provider amounts: [cost](docs/cost.md)
  - providers: [adapters](docs/adapters.md), [provider-onboarding](docs/provider-onboarding.md), [realtime](docs/realtime.md)
  - decisions: [docs/adr/](docs/adr/); reproducibility claims: [determinism](docs/determinism.md)

## Conventions that differ from defaults

- Kaana's canonical signed origin is `https://kaana.ai`; never add a Kaana hostname
  under `oxy.so`. Identifiers use `kaana`, `KAANA_*`, `X-Oxy-Kaana-*`.
- The contract source is `packages/contracts` (Zod), flowing Zod -> descriptor -> Go.
  Regenerate `internal/contract/descriptor.json` and `packages/contracts/openapi.json`
  (`make contract`); never hand-edit them, they are byte-exact outputs.
- Biome covers only `packages/**` and `tools/**` (ADR 0002). Widening it would rewrite
  `configs/**`, `.github/*.json` and the generated files.
- The model catalogue is `configs/public-catalogue.json`, Kaana's alone; add no field
  without primary evidence ([operating](docs/operating.md#the-declared-catalogue)).
- An Oxy id is opaque: never parse it, join on it or use it as a primary key.
- No `TODO`/`FIXME`/shims; wrap errors for `errors.Is`; Conventional Commits.

## Boundaries

Never:
- put a provider key, endpoint secret or Oxy secret in the repo, a test, CI or an env
  var; keys live only as PostgreSQL KMS ciphertext ([key-pools](docs/key-pools.md)).
- persist or log a prompt (images included), a completion or a user IP: it is customer data.
- attempt a route Oxy did not sign, or add a field the contract lacks (`notApplicable`
  for unexchanged shapes): callers and Kaana would diverge.
- let an amount reach an inference response: only `internal/providercost` holds one.
- run `npm publish` locally: merging a version bump to `main` publishes it through
  `release-packages.yml` ([npm releases](https://github.com/OxyHQ/oxy/blob/main/docs/npm-releases.md)).

Ask first: widening the contract, adding a provider, changing breaker or retry
semantics, or touching the key store schema.

## Testing

- Run `make check` before opening a PR; it is the definition of done, and CI runs the same.
- Credential store changes also need `make test-integration`.
- A new gate needs a positive control; mutation-test what is load-bearing
  ([docs/rules.md](docs/rules.md#tests), [gate writing](https://github.com/OxyHQ/oxy/blob/main/docs/engineering/gate-writing.md)).

## PRs and commits

Conventional Commits, one concern per PR, branch from `main`. Merged is not deployed:
say whether a rollout, migration or canary has run ([inference-rules](docs/inference-rules.md#source-state-is-not-production-state)).

## Secrets

None are needed to build or test. `KAANA_POSTGRES_TEST_URL` comes from your own local
Postgres; never print it or any `*_DATABASE_URL`.

## Links

[Oxy engineering standards](https://github.com/OxyHQ/oxy/blob/main/docs/engineering/standards.md),
[lint and format](https://github.com/OxyHQ/oxy/blob/main/docs/engineering/lint-and-format.md),
[infra and deploys](https://github.com/OxyHQ/oxy-infra) (private).

Owner: Kaana maintainers. Last reviewed: 2026-11-11.
