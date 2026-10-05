# System One Score mass normalization

The OpenRouter/TypeSafe adapter now normalizes a Score distribution only after
its raw probabilities pass the existing finite, [0,1], exact-level and mass
1±1e-6 checks. It preserves the provider's score, reply and confidence. The
unchanged contract checks that score against the normalized weighted mean with
the same 1e-6 tolerance. Choice and Noul are unchanged. Invalid distributions,
zero mass and inconsistent scores remain failures with measured usage/cost.

This corrects a reproducible representation mismatch, not a demonstrated
reconstruction of Mention's lost response. The second production request
f1f5293a-370c-47b9-b176-b973748d05b6 failed with the finite reason
`score_distribution`; no response values were retained. This change neither
replays it nor authorizes another request.

## Primary evidence

- [TypeSafe Score semantics](https://docs.typesafe.ai/primitives/score): the
  score is a probability-weighted position on the ordered levels.
- [Official TypeSafe adapter at e1d4cc](https://github.com/typesafe-ai/system-one-adapter-python/blob/e1d4cc938204b22fc5a3c3aca7044072fe3f712d/src/system_one_adapter/_client.py):
  `_convert_llm_value_to_typesafe_answer` computes Score using rescaled
  probabilities while it can return the original probabilities.
- [Its normalization utility](https://github.com/typesafe-ai/system-one-adapter-python/blob/e1d4cc938204b22fc5a3c3aca7044072fe3f712d/src/system_one_adapter/_utils/probability_normalization.py)
  defines a 1e-6 probability-mass tolerance. We do not adopt its zero-mass fallback.

That project adapts other LLMs to TypeSafe's interface; it is not the source of
Jev's hosted implementation and does not establish the historical failure's
numeric magnitude. No published rounding rule supports widening our tolerance.

## Validation

The same 13-case real HTTP fixture fails three cases against baseline 112afea:
near-unit mass on either side is incorrectly rejected, while a score derived
from the unnormalized mass is incorrectly accepted. The corrected adapter
preserves the original score/reply, normalizes the distribution, and passes
serialization through the existing Go contract. Negative cases retain exact
level keys, finite/range/mass guards and cost/usage on failure. Noul remains
verbatim. Published-contract replay and the full canonical gates are recorded
in proof.json. These are synthetic fixtures, not captured production answers.

No authority, model, policy, source approval, request count, rate card, SDK
version, public/private route or diagnostic logging policy changes.
