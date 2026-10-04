# Authenticated decision-model discovery

The OpenRouter model list defaults to text. The publisher therefore could not
find the exact Jev decision identity even when its reviewed permit, card and
key were present. The existing authenticated GET now requests
`output_modalities=text,decisions`; no second request or approval fallback is added.

The retained public responses contain 466 default models and 479 expanded
models. The 13 additional IDs have no current ordinary attribution. The local
HTTP regression reproduces the missing private candidate on the baseline,
then passes through real Discover, ZDR filtering, scopedCandidate and snapshot
construction. The ordinary snapshot is byte-identical after excluding the 13
new IDs; exactly one scoped candidate is appended with the explicit synthetic
permit. Missing Jev ZDR rejects it, and another provider retains its original
request/authentication. Public fixtures do not establish account eligibility.

Publisher race tests, full Go race suite, build, vet and final lint pass.
Initial fixture Close lint findings and baseline missing-candidate failure are
retained. The full race run preceded only the test-helper Close error-reporting
cleanup; the final publisher suite and lint were repeated. No private SQL,
provider inference, AWS operation, push or deployment was performed. Existing
cards, expiry, source audience and ordinary attribution remain byte-identical.

`proof.json` binds sources and records; compressed fixtures preserve the exact
public response bytes and `provenance.json` records their URLs and hashes.
