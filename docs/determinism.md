# Determinism and reproducibility

What Kaana can and cannot promise about getting the same answer twice. Written
after reading Thinking Machines Lab's "Defeating Nondeterminism in LLM
Inference" (2025-09-10); their figures are theirs and are cited only as the
reason for the rules below.

## Why `temperature: 0` and `seed` are not a guarantee

A forward pass is run-to-run deterministic on fixed hardware. The answer still
varies because many kernels choose their reduction order from the batch size,
and the batch size is set by the provider's load at that moment. TML measured
80 distinct completions out of 1000 identical greedy requests on one model;
only batch-invariant kernels removed the variance, at roughly 1.6x to 2.1x the
latency on their benchmark. Kaana executes against third-party providers, so
it controls none of the kernels, engine version, hardware or load.

## What Kaana promises

- **The exact model revision.** A pinned `publisher/model@revision` is never
  substituted; a route switch is announced, never silent (`routing.md`).
- **Provenance.** Every answer names the model and route that produced it.
- **No false claim.** `seed` is forwarded only where the provider documents it
  and is refused where `Translate` cannot express it (`provider-onboarding.md`).
  Documentation, SDKs and dashboards must not describe `seed` or
  `temperature: 0` as reproducible.

## What Kaana does not promise

- Bitwise or token-identical output across calls, routes, keys, days or
  provider engine versions. A provider may change an engine without notice.
- That sticky routing reduces variance: the cause is load-driven batch size.

## Rules a reviewer applies

- A test that talks to a real provider never asserts exact text equality across
  two calls. Compare with tolerance (common-prefix length, a structural or
  semantic check). Exact equality belongs only to the real-wire fakes.
- A change that adds a "reproducible" or "deterministic" claim to a doc, SDK
  comment or response needs the evidence for the specific provider.
- A consumer feeding rollouts into training must record the sampler's own
  logprobs and correct for the sampler/trainer mismatch itself; Kaana does not
  assert they match its trainer.
