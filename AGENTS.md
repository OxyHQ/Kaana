# Kaana

Kaana is Oxy's inference **data plane** (Go); Oxy is the single control plane.
Kaana executes signed, already-authorized requests and never owns accounts,
credentials, a ledger or a console. Alia is the agent runtime, not this repo:
`app -> Oxy edge -> Kaana` or `app -> Alia -> Oxy edge -> Kaana`.

## Commands (CI)

```bash
gofmt -l .                      # prints nothing
go build ./... && go vet ./...
golangci-lint run               # v2.13.2
go test -race -count=1 ./...
cd tools/contract && bun install --frozen-lockfile && bun run generate && bun run validate
```

## Rules

Boundary — docs/architecture.md#rules-a-reviewer-applies
- An Oxy id is opaque: never parsed, joined or a primary key.

Contract — docs/contract.md#rules-a-reviewer-applies
- Never edit `internal/contract/descriptor.json` by hand; regenerate it.
- Never add a field the contract lacks; unexchanged shapes go in `notApplicable`.

Routing — docs/routing.md#rules-a-reviewer-applies
- Attempt only the signed `authorizedRoutes`, in order; inventory authorizes none.
- A `RouteSet` is one model reference; build `provider.Route` only in `Candidates()`.
- Only `provider.AttributableCategory` trips a breaker; half-open admits one request.

Inventory — docs/inventory.md#rules-a-reviewer-applies
- Re-issue inside the horizon; staleness is the snapshot's own `issuedAt`.
- The revision label is carried forward per model line, never re-dated.
- An upstream id must name the same weights tomorrow: no routers, aliases, `:batch`.
- Validate with `inventory.Parse` before writing; never default the bucket.

Credentials — docs/key-pools.md#rules-a-reviewer-applies
- PostgreSQL KMS ciphertext is the only key store; plaintext only on stdin.
- Rotate on reported exhaustion or rejection; a request fault never walks the pool.
- Verdicts come from the adapter's code, never a status; a 402 retires the key.
- BYOK: same custody, inverse authorities, one exact operation id —
  docs/customer-provider-credentials.md#rules-a-reviewer-applies

Cost — docs/cost.md#rules-a-reviewer-applies
- Only `internal/providercost` holds an amount; none enters a response.

Adapters — docs/adapters.md#rules-a-reviewer-applies
- One `provider.Adapter` and one real-wire fake per provider; pass conformance.
- Refuse in `Translate` what the provider cannot express; invent no default.
- Classify by the provider's error type; redact your own key by exact match.
- `Stream` returns measured units even on failure; `ctx` reaches upstream.

Cross-cutting — docs/rules.md
- No secret in repo, test, CI or env; none in a log, error or usage record.
- No prompts, completions or user IPs persisted or logged.
- Every gate needs a positive control; mutation-test what is load-bearing.
- No `TODO`/shims; Conventional Commits; wrap errors for `errors.Is`.

## Before touching X, read `docs/`

schema→architecture · envelope→contract · executor→routing · publisher→inventory ·
keys→key-pools, customer-provider-credentials · provider→adapters,
provider-onboarding · deploy→operating.
