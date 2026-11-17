# Provider credential destruction — 2026-11

> **Rollout record.** This describes one cleanup as planned on 2026-11-17. It is not a rule
> and is not kept current; the rules are in [key-pools](key-pools.md#destroying-a-dead-key)
> and the executable source of truth is `.github/credential-admin-operations.json`.

The production `list` of 2026-11-14 holds ten disabled rows. Nothing temporary
disables a key (a spent or refused key is *retired* with a return time; a failing
deployment is the breaker's), so each of them is a key that was replaced or
superseded, and each still holds its KMS ciphertext. This cleanup removes those
ciphertexts with `kaana-credentials destroy` (migration `0025`) and keeps every
identity, so cost, attempt, metadata and audit history keep resolving.

## The ten operations

| Operation | Provider | Key id | Why it is disabled | Expected proof |
|---|---|---|---|---|
| `destroy-anthropic-b46cc8c4` | `anthropic` | `b46cc8c4-736e-4e7e-b3e4-be74c0d75f66` | no record in this repository | the provider's verdict |
| `destroy-cerebras-relay-main` | `cerebras` | `cerebras-relay-main` | legacy name, rekeyed to `43405cea-…` | duplicate of `43405cea-…` |
| `destroy-elevenlabs-legacy-alia` | `elevenlabs` | `legacy-alia-20260901` | Alia's key, rekeyed to `6e4abb22-…` | the provider's verdict |
| `destroy-elevenlabs-6e4abb22` | `elevenlabs` | `6e4abb22-af03-46fb-95d9-b2e4286657f2` | replaced by the Startup Grant key `e1ed4df9-…` | the provider's verdict |
| `destroy-groq-legacy-alia` | `groq` | `legacy-alia-20260901` | rekeyed to `8295090b-…` | duplicate of `8295090b-…` |
| `destroy-groq-relay` | `groq` | `relay-groq-20260902` | deduplicated against the Alia key | duplicate of `8295090b-…` |
| `destroy-openrouter-legacy-alia` | `openrouter` | `legacy-alia-20260901` | rekeyed to `b8090dce-…` | duplicate of `b8090dce-…` |
| `destroy-openrouter-relay` | `openrouter` | `relay-openrouter-20260902` | deduplicated against the Alia key | duplicate of `b8090dce-…` |
| `destroy-xai-legacy-alia` | `xai` | `legacy-alia-20260901` | rekeyed to `1d72d527-…` | duplicate of `1d72d527-…` |
| `destroy-xai-relay` | `xai` | `relay-xai-20260902` | deduplicated against the Alia key | duplicate of `1d72d527-…` |

"Expected" is a reading of the cutover records (a rekey copies the same secret under
the new id; the `deduplicate-*` operations ran and no `*-secondary-if-different` id
exists), not evidence. The command decides, and a row that proves neither is left
exactly as it is.

The exact commands, fixed in the manifest and dispatched by name from `main`:

```text
kaana-credentials destroy --operation-id kcd_a6776a5c762fa23bff8eccfac41040c8 --provider anthropic --key-id b46cc8c4-736e-4e7e-b3e4-be74c0d75f66
kaana-credentials destroy --operation-id kcd_7b5b670af00bc17fff7ac4f3d47ea6f7 --provider cerebras --key-id cerebras-relay-main
kaana-credentials destroy --operation-id kcd_934ed4f25f7ad1ef62b0165a7dda825f --provider elevenlabs --key-id legacy-alia-20260901
kaana-credentials destroy --operation-id kcd_ad20b03078706cf4a89bce1deb28fc10 --provider elevenlabs --key-id 6e4abb22-af03-46fb-95d9-b2e4286657f2
kaana-credentials destroy --operation-id kcd_668fa053b78f7784bd6dc543d41c8eb9 --provider groq --key-id legacy-alia-20260901
kaana-credentials destroy --operation-id kcd_85dabf9f40fcba26750284e2e733071e --provider groq --key-id relay-groq-20260902
kaana-credentials destroy --operation-id kcd_ded7d342f1f2b8c3131b426753eb2866 --provider openrouter --key-id legacy-alia-20260901
kaana-credentials destroy --operation-id kcd_c7a5bf74f09a031cad714c0b9aaacfc7 --provider openrouter --key-id relay-openrouter-20260902
kaana-credentials destroy --operation-id kcd_38b04461957ef6d2d810bb24235fa291 --provider xai --key-id legacy-alia-20260901
kaana-credentials destroy --operation-id kcd_eed6066329848e4d6b532e29fb0c7e77 --provider xai --key-id relay-xai-20260902
```

## Gates, in order

1. **Merge and deploy.** The deploy applies migration `0025` before it moves a service
   (`operating.md`). The destroy operations run the image built from the dispatched
   `main` commit, like `migrate`, because the pinned administration image predates them.
2. **Grant decryption for the cleanup only.** On 2026-11-17 the task role
   `oxy-kaana-credential-admin` holds `kms:Encrypt` alone; `destroy` must decrypt the
   candidate and the enabled keys it compares with. An oxy-infra change grants
   `kms:Decrypt` on the one credential key ARN for the cleanup, and is reverted after it.
   Without it every operation fails closed at KMS and changes nothing.
3. **Dispatch the ten operations.** Each prints a receipt with its proof. A refusal is an
   answer, not a failure to retry around:
   - `the provider still accepts this secret`: the key is alive. Revoke it in that
     provider's console, then dispatch the same operation again (it is idempotent).
   - `the provider did not prove this secret invalid`: no reviewed probe for the provider,
     or an ambiguous answer. Leave the row; it still cannot serve.
4. **Read back** with `list` (destroyed rows no longer appear) and revert the grant.
5. **Retire the ten operations** from the manifest and workflow in a cleanup PR, as the
   2026-09-11 handoffs were.

ElevenLabs keys are restricted to Kaana's NAT egress addresses, so its probe is only
meaningful from the administration task, never from an operator's machine.
