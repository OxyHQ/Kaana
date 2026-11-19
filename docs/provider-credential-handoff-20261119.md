# Provider credential handoff — 2026-11-19

**Completed 2026-11-19.** `handoff-import-anthropic` imported the key and
`handoff-describe-anthropic` applied `kcm_c47e220d252a6545c3e24bcef993352e`;
`list` read the row back enabled at position 2. Both SSM parameters and the
local copies were deleted, and the operations, the two `ssm.go` map entries,
the `put-credential-metadata --parameter` path and the IAM grant were retired.
A new handoff re-adds them from this record's change (OxyHQ/Kaana#341).

A temporary, closed handoff for one Anthropic key: the monthly API credit that
comes with the owner's subscription (200 USD a month). It is a rollout record,
not a rule. Cleanup is part of the handoff, not a follow-up.

| Operation | Provider | Key id | Position | Funding |
|---|---|---|---|---|
| `handoff-import-anthropic` | `anthropic` | `7e9c2e7e-fffb-4389-bec6-59647882ff2c` | 2 | `promotional` (from its metadata) |
| `handoff-describe-anthropic` | `anthropic` | same key | — | — |

Position 2 is the next free enabled position (`30033d0c-…` holds 1). It does
not decide the order here: the pool orders by funding first
([key-pools](key-pools.md)), so once described as `promotional` this key is
spent before the unstated `30033d0c-…`, and it is the only Anthropic key
admitted under a credit ceiling. The credit expires each month unspent, which
is why it goes first.

The executable commands are fixed in `.github/credential-admin-operations.json`
and run on the image built for the dispatched `main` commit:

```text
kaana-credentials import-ssm --provider anthropic --key-id 7e9c2e7e-fffb-4389-bec6-59647882ff2c --position 2 --parameter /oxy/kaana/provider-key-handoff/20261119/anthropic
kaana-credentials put-credential-metadata --provider anthropic --key-id 7e9c2e7e-fffb-4389-bec6-59647882ff2c --parameter /oxy/kaana/provider-key-handoff/20261119/anthropic-metadata
```

## Why the metadata crosses SSM too

The description names its funding account's protected label, and this
repository is public. `put-credential-metadata` otherwise reads stdin, which a
workflow-dispatched ECS task does not have. With `--parameter` it reads one
reviewed SecureString bound to exactly this `(provider, keyId)` and refuses a
document describing any other key. The document is compact JSON (one line,
under 4096 bytes):

- `title`: "Anthropic subscription credit, 200 USD/month";
- `fundingAccount`: a fresh `kfa_` id and the account's protected label;
- `capacityCategory`: `promotional`; `environment`: `production`;
- `commercialUse`: `permitted`, on the owner's statement of 2026-11-19 that
  this key's credit may serve Kaana's commercial traffic;
- no `restrictions`.

## Release and execution gates

1. Merge this change and let `main` build its image.
2. Merge and apply the matching oxy-infra change: `ssm:GetParameter` on exactly
   the two handoff ARNs for `oxy-kaana-credential-admin`, nothing broader.
3. Stage both SecureStrings with `alias/aws/ssm` from local files, never
   printing them; confirm only their metadata.
4. Dispatch `handoff-import-anthropic`, then `handoff-describe-anthropic`.
   Until the second runs the key is unstated and spent last, so run them back
   to back. Read back with `list`: the row is enabled at position 2.
5. Delete both SSM parameters and the local source files. Then merge and apply
   the cleanup: remove both operations, the two map entries in
   `internal/credentialstore/ssm.go` and the IAM grant.
