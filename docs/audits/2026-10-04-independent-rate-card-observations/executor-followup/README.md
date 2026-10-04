# Executor follow-up: original checkpoint incomplete

Root review found the executor still used the single global observation. With
both cards, it rejected the approved private deployment before any claim/send.
The exact new executor fixture is RED against that original code; only the own
card fails, while foreign-version and mismatched-source refusals pass. The lookup
now resolves the signed deployment observation, and the same fixture succeeds
with one claim/send and its own cost version. All scoped executor negatives and
final make check pass. No source getter/card/task activation is included.
The earlier source/proof checkpoint is retained and is not complete scoped
execution acceptance. The initial fixture compile error is setup-only history.
