# Cross-cutting rules

The rules that have no single subsystem: what may never be persisted or
emitted, and what a test has to prove before it counts. Every other rule lives
at the end of its topic document under "Rules a reviewer applies";
`../AGENTS.md` is the one-line index to all of them.

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
  the upstream really echoed one (`adapters.md`, "Rules a reviewer applies").
- **Prompts and completions are not persisted and never enter a log line.** Log
  ids, a route, an outcome and a duration.
- **Never persist a user IP** — raw, hashed or geo-derived. The contract's
  `client` metadata block is `.strict()` for exactly this reason; do not widen it.

## Tests

- **A gate needs a positive control.** Ask what the check would report if the
  thing it measures were absent. If that is the same answer, it measures nothing.
  `drift_test.go` and the invalid fixtures in `validate.mjs` are the pattern
  (`contract.md`).
- **A cancellation or propagation test needs an uninterrupted control run.**
  "The upstream saw its caller go away" is also what a request that finished
  reports (`routing.md`, "Cancellation").
- **Mutation-test a load-bearing assertion**: apply the mutation, confirm it
  applied (a mutation that never applied is indistinguishable from one that
  survived), watch the test fail, restore, watch it pass.
- **Populate optional fields in a fixture.** A field that drifted is invisible
  in a minimal one.
- **Exact counts, not floors**, for exemption lists and fixture sets: a floor
  erodes one defensible line at a time.

## Working here

- Run the commands CI runs (the block in `../AGENTS.md`), not near-equivalents.
- No `TODO`, `FIXME`, `HACK`, no back-compat shims, no deprecated aliases.
  Breaking changes are clean cuts.
- Conventional Commits.
- Errors are wrapped and matched with `errors.Is`/`errors.As`; the executor's
  control flow depends on it.
- Say what is *not* implemented rather than stubbing it. The README's
  out-of-scope list is part of the deliverable.
