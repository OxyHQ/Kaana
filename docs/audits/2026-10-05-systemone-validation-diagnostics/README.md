# SystemOne validation diagnostics

The paid Mention request `40fcb042-cd51-4754-88e8-273230a0f761` failed with a generic `invalid systemone response`. Its response body was not retained, so this change does not claim a historical cause or a repaired wire incompatibility.

The actual Mention request has three Noul answers and one five-level Score. A synthetic response matching the documented shape, including Score `legend`, already passes the original parser. Primary references: [TypeSafe Score](https://docs.typesafe.ai/primitives/score), [Noul](https://docs.typesafe.ai/primitives/noul), [OpenRouter TypeSafe SDK](https://openrouter.ai/docs/guides/community/typesafe-sdk). The existing validator remains byte-identical, including probability and weighted-mean tolerances.

Parser failures now choose a finite reason. The existing provider error carries that reason through `ContractError`; the HTTP result logger projects only an exact allowlist into `decisionResponseValidation`. Raw errors, question IDs, responses, answers, model strings, numeric values and secret-like suffixes never enter this new log field. Unrecognized validator messages use the fixed `answer_contract` fallback. Failure code/category, units and provider cost remain unchanged.

The identical synthetic malformed responses produce 19 diagnostic failures on the baseline and pass after the change while remaining rejected, retaining input618/output72/request1 and USD0.000025956. The independent log regression fails its positive control before logger wiring; arbitrary detail, prefixed secret, suffix and newline controls stay excluded. All `make check` targets ran in three recorded groups with pinned Go/lint/Bun versions; no database integration run was needed or claimed. See [proof.json](proof.json) for frozen source and log hashes.

No source approval, request identity, expiry, rate card, contract descriptor or authorization changes. No production operation or provider request was performed. A future separately reviewed operation can reveal a reason; the historical request must not be replayed.
