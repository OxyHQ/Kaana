# Provider credential ID cutover

Platform provider credentials use opaque IDs. Historical IDs containing product
or transport names remain only on disabled PostgreSQL rows and audit records;
they are never selected by runtime configuration after this cutover. No
provider secret leaves KMS/process memory during the operation.

The executable source of truth for every operation ID and exact source/target
pair is [`.github/credential-admin-operations.json`](../.github/credential-admin-operations.json).
The workflow accepts only those complete argument arrays; it has no free-form
input. The manifest pins the immutable image produced from main commit
`728cf22b18042e3f8e7e9d68d6fb44a18756d6b4` by build-only run `33715928656`.
Before every operation, the workflow selects the task profile named explicitly
for that operation, validates its current shape, replaces only its image with
that digest, registers a new revision and compares the full normalized readback
with the derived document. `migrate` alone selects the DDL-capable migrator
profile and `/oxy/kaana/MIGRATOR_DATABASE_URL`; every list, comparison and rekey
selects the credential-admin profile and its separate database principal.

## Canonical IDs

| Provider | Primary ID | Secondary ID when the exact comparison says `different` | Discovery/runtime |
|---|---|---|---|
| Cerebras | `43405cea-a7d1-49c2-ba73-5a84536d3abf` | none | primary |
| Groq | `8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1` | `f0c4e09f-a5f8-4af8-86b4-960e2d637ce1` | primary |
| OpenRouter | `b8090dce-82f2-4077-9fc1-fd831a53ca27` | `2bdf7141-fdf6-4cbf-8332-3ea98202f52f` | primary |
| xAI | `1d72d527-81ca-41e5-9644-2d81a4b126ec` | `ad05516d-e2d2-4be4-8735-5e69c9bff41c` | primary |
| ElevenLabs | `6e4abb22-af03-46fb-95d9-b2e4286657f2` | none | not configured; there is no proven adapter/routing path |

The secondary IDs are reserved even when comparison returns `deduplicated`.
They must not be created in that case. Pool position is copied from the exact
source row by the database operation; it is never a selector.

## Transaction and receipt

Migration `0007` adds two internal operations:

- `rekey-id` locks the provider administration lane, selects the source by the
  exact `(provider, oldKeyId)` primary key, proves the destination is absent,
  decrypts with the old KMS context, encrypts with the new context, disables the
  old row, inserts the new row with the same class/budget/position, writes both
  audit records and commits one terminal receipt. KMS failure or any database
  error rolls the whole PostgreSQL transaction back.
- `deduplicate` locks and decrypts two exact IDs, compares fixed-size buffers in
  process memory, and sends PostgreSQL only an equality bit. `deduplicated`
  disables the declared duplicate. `different` changes no credential. Neither
  plaintext nor a digest/fingerprint is stored or printed.

Disabled source ciphertext remains in the base table as history, but migration
`0007` revokes runtime access to that table and the administrator's metadata
views do not expose ciphertext. Serving and publisher read a security-barrier
view containing active rows only.

Every operation ID is bound permanently to its action, provider, source,
destination and prerequisite receipt/outcome. An exact replay returns the original receipt without another KMS
call. A different GitHub run may recover that receipt; the audit actor remains
the actor that first committed it, and the operation ledger records the original
database session principal. Reusing an operation ID for another selector fails
closed. An existing destination without that exact receipt also fails closed,
even when the row is disabled.

The credential-admin task needs `kms:Decrypt` and `kms:Encrypt` on the one exact
Kaana key for this short-lived operation. `Encrypt` alone is insufficient:
`deduplicate` must decrypt both exact source rows, while `rekey-id` must decrypt
the source before encrypting the destination under its new context. A denied
decrypt aborts the operation and its PostgreSQL transaction; it must never be
handled as a different credential or compensated by copying ciphertext.
Serving/publisher authority does not change. Apply migration `0007` with the
migrator identity first, publish a new immutable admin image, update its pinned
digest in the reviewed manifest, and read the task definition and its temporary
exact-key `kms:Decrypt`/`kms:Encrypt` permissions back before running anything.
Remove those temporary admin permissions after the reviewed operation set and
verification finish. No command below carries a secret in argv, environment,
stdout or the workflow manifest.

## Fail-closed release order

The normal AWS workflow is deliberately gated by the exact repository variable
`KAANA_PROVIDER_CREDENTIAL_ID_CUTOVER_COMPLETE == 'true'`. An absent variable
is false. The same gate applies to automatic pushes and manual dispatches, and
manual dispatch additionally requires `refs/heads/main`.

Keep that variable absent or false while this source is merged. Run the fixed
`migrate` operation first; its workflow promotes and proves the pinned migrator
task, dedicated execution role and DDL database principal before applying
migration `0007`. Grant the separate short-lived admin task only
`kms:Decrypt` and `kms:Encrypt` on the exact Kaana key before the comparison and
rekey operations, run the fixed dedupe/rekey operations below, and read every
receipt and final row identity back. Update publisher discovery configuration
to the exact canonical IDs only after those rows exist. Then set the repository
variable to the literal `true`, read it back, and dispatch the deploy workflow
from the exact main commit with mode `deploy`. Setting the variable is not a
deployment and never substitutes for the subsequent ECS image/task-definition
and live provider verification.

If any prerequisite fails, leave the gate false and the running services
untouched. Never make a retry possible by weakening the workflow condition.

## Exact order and commands

Run comparisons before any rekey. This makes the live duplicate explicit and
avoids moving a duplicate into two new identities. The three primary rekeys
carry the exact comparison operation as a prerequisite, and each secondary also
requires that receipt's exact `different` outcome; PostgreSQL refuses an
out-of-order or wrong-branch command even though the workflow lists it.

```bash
kaana-credentials deduplicate --operation-id kop_0af8007d9fdddd88d2622eabff99aeb9 --provider groq --duplicate-key-id relay-groq-20260902 --keep-key-id legacy-alia-20260901
kaana-credentials deduplicate --operation-id kop_64722ac4d450f4ac2d5c6b6bd0fe0a15 --provider openrouter --duplicate-key-id relay-openrouter-20260902 --keep-key-id legacy-alia-20260901
kaana-credentials deduplicate --operation-id kop_29de63b8cd98855b8e0a440d9db7aef3 --provider xai --duplicate-key-id relay-xai-20260902 --keep-key-id legacy-alia-20260901
```

For each receipt, record only its JSON. If `outcome` is `deduplicated`, do not
run that provider's secondary command. If it is `different`, both exact rows are
still enabled and the reserved secondary rekey is required.

Then rekey the five primary rows:

```bash
kaana-credentials rekey-id --operation-id kop_5b4f96c394a7a288754a1388fed0c5b2 --provider cerebras --old-key-id cerebras-relay-main --new-key-id 43405cea-a7d1-49c2-ba73-5a84536d3abf
kaana-credentials rekey-id --operation-id kop_3ac18ed3ab6c6bf97862b03193ef4357 --provider groq --old-key-id legacy-alia-20260901 --new-key-id 8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1 --requires-operation-id kop_0af8007d9fdddd88d2622eabff99aeb9
kaana-credentials rekey-id --operation-id kop_eb9b5b291df58b7573633e92f5eb8ad4 --provider openrouter --old-key-id legacy-alia-20260901 --new-key-id b8090dce-82f2-4077-9fc1-fd831a53ca27 --requires-operation-id kop_64722ac4d450f4ac2d5c6b6bd0fe0a15
kaana-credentials rekey-id --operation-id kop_6f4d191e8834c4410049904de37952a6 --provider xai --old-key-id legacy-alia-20260901 --new-key-id 1d72d527-81ca-41e5-9644-2d81a4b126ec --requires-operation-id kop_29de63b8cd98855b8e0a440d9db7aef3
kaana-credentials rekey-id --operation-id kop_b5d7eca4d16b7162529ab4688042efae --provider elevenlabs --old-key-id legacy-alia-20260901 --new-key-id 6e4abb22-af03-46fb-95d9-b2e4286657f2
```

Only for a provider whose comparison returned `different`, run its exact
secondary rekey:

```bash
kaana-credentials rekey-id --operation-id kop_c1b4d87bf4e2a5a6d815dc1a1b0460a3 --provider groq --old-key-id relay-groq-20260902 --new-key-id f0c4e09f-a5f8-4af8-86b4-960e2d637ce1 --requires-operation-id kop_0af8007d9fdddd88d2622eabff99aeb9 --requires-outcome different
kaana-credentials rekey-id --operation-id kop_0418afb5cc61a79a8ff2db4ddcd5b809 --provider openrouter --old-key-id relay-openrouter-20260902 --new-key-id 2bdf7141-fdf6-4cbf-8332-3ea98202f52f --requires-operation-id kop_64722ac4d450f4ac2d5c6b6bd0fe0a15 --requires-outcome different
kaana-credentials rekey-id --operation-id kop_49a92662d24e3190eaa25e0396780e29 --provider xai --old-key-id relay-xai-20260902 --new-key-id ad05516d-e2d2-4be4-8735-5e69c9bff41c --requires-operation-id kop_29de63b8cd98855b8e0a440d9db7aef3 --requires-outcome different
```

The production workflow exposes these same commands as fixed choices. Prefer
it to an interactive shell: its task identity, image digest, database secret
binding, KMS ARN, command and audit actor are all attested before execution.

## Verification and retirement

1. Run `list` and require each expected canonical ID to be enabled and every
   exact historical source to be disabled. There must be no unplanned ID.
2. Set each publisher `KAANA_PROVIDER_<SLUG>_DISCOVERY_KEY_ID` to the primary ID
   in the table above. ElevenLabs remains absent from discovery and serving.
3. Reload and require the complete pool to decrypt. Run one real catalogue read
   and one real inference per configured provider; a database receipt is not a
   provider-call proof.
4. Only after those checks, delete the exact legacy SSM SecureStrings and their
   GitHub copies, remove the one-shot import IAM grant, and remove the importer
   in a later clean-cut migration. The reviewed workflow no longer exposes an
   import choice, so it cannot reactivate a historical ID after this cutover.

Never copy ciphertext between IDs, re-enable a historical row independently,
or compensate with `put`: KMS context and the atomic receipt are the authority.
Before commit, failure is ordinary transaction rollback and leaves the source
active. After commit, stop and recover through a separately reviewed exact
operation; do not invent an ID or select a row by provider name, position or
database order.
