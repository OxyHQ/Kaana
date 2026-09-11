# Provider credential handoff — 2026-09-11

This is a temporary, closed handoff for five locally held provider keys. Kaana
owns only credential custody and runtime key identity. Account title, trial,
balance and commercial-rights evidence belong to Oxy/infra and must not be
invented as Kaana routing or commercial policy.

| Operation | Provider | Opaque key ID | Position | Kaana class/budget |
|---|---|---|---:|---|
| `import-handoff-cheaperinference` | `cheaperinference` | `e97a886e-ab58-4492-b250-84a944e44276` | 1 | `paid`, budget `0` |
| `import-handoff-mistral` | `mistral` | `fcb72e20-6b68-418f-bf34-50b58e59744e` | 1 | unstated |
| `import-handoff-cohere` | `cohere` | `3574baf0-c7b8-4985-bc5f-94d29b72eafb` | 1 | `free` |
| `import-handoff-cohere-2` | `cohere` | `5db11d45-b08a-4b2a-a318-3f88f5d8466a` | 2 | `free` |
| `import-handoff-openai` | `openai` | `610adcc8-4a29-4ab1-a1c8-fca191a1fadd` | 1 | `paid`, budget `0` |

The executable commands are fixed in
`.github/credential-admin-operations.json`; the workflow has no free-form
provider, path, ID, position or metadata inputs. They are exactly:

```text
kaana-credentials import-ssm --provider cheaperinference --key-id e97a886e-ab58-4492-b250-84a944e44276 --position 1 --parameter /oxy/kaana/provider-key-handoff/20260911/cheaperinference --class paid --budget-usd 0
kaana-credentials import-ssm --provider mistral --key-id fcb72e20-6b68-418f-bf34-50b58e59744e --position 1 --parameter /oxy/kaana/provider-key-handoff/20260911/mistral
kaana-credentials import-ssm --provider cohere --key-id 3574baf0-c7b8-4985-bc5f-94d29b72eafb --position 1 --parameter /oxy/kaana/provider-key-handoff/20260911/cohere --class free
kaana-credentials import-ssm --provider cohere --key-id 5db11d45-b08a-4b2a-a318-3f88f5d8466a --position 2 --parameter /oxy/kaana/provider-key-handoff/20260911/cohere-2 --class free
kaana-credentials import-ssm --provider openai --key-id 610adcc8-4a29-4ab1-a1c8-fca191a1fadd --position 1 --parameter /oxy/kaana/provider-key-handoff/20260911/openai --class paid --budget-usd 0
```

## Release and execution gates

1. Merge this code, let the build-only workflow publish it, then separately pin
   that exact digest and source commit in the reviewed operations manifest.
2. Merge and manually apply the matching infra PR. Read back the task role and
   require `ssm:GetParameter` on exactly the five handoff ARNs—no prefix,
   wildcard, path listing, GitHub secret or execution-role secret injection.
3. Stage each local file through the infra-owned operator tool. It reads the
   file directly and calls SSM; the value never appears in argv, stdout, an
   environment variable, Terraform state or GitHub. Confirm only SSM metadata.
4. Dispatch the five fixed workflow choices from `main`. A successful import is
   not provider evidence. Check the five exact rows with `list`, then make a
   reviewed real request only where Oxy has an enabled deployment and valid
   commercial policy. CheaperInference's zero balance and OpenAI's unverified
   balance are not usable capacity.
5. Delete all five exact SSM parameters and securely remove the five local
   source files only after ciphertext/readback and any authorized provider call
   are verified. Then immediately merge/apply a cleanup PR removing the five
   code entries, workflow operations and exact IAM grants. Cleanup is part of
   the handoff, not optional follow-up.

If any identity, type, metadata or evidence differs, stop. Do not change an ID,
reuse one Cohere row, widen either allow-list, or substitute `put`.
