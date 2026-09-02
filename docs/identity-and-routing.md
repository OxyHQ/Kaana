# Identity and request routing

This is the canonical short answer to three questions that used to be mixed
together: what Kaana is, what Alia is, and where an Oxy product sends an AI
feature.

## One name and one origin

The inference data plane is **Kaana**. Its repository is `OxyHQ/Kaana`, its
configuration uses `KAANA_*`, and its only canonical signed data-plane origin is
[`https://kaana.ai`](https://kaana.ai).

The former inference-service name is not an internal compatibility identity.
Historical `Relay` task, SSM and environment names may appear only in one-time
migration code or records that explain their retirement. They must not appear
in a new route, hostname, package, task, model alias or public instruction.
Unrelated uses of the ordinary word "relay" — SMTP, federation or device
transport — are not Kaana and must not be renamed.

The old Alia provider aliases are Kaana concerns too. This does **not** rename
the Alia product: it removes provider selection, provider secrets and generic
inference execution from Alia while preserving Alia's agent runtime.

## Kaana is not Alia

| System | Owns | Does not own |
|---|---|---|
| **Kaana** | provider adapters, authenticated provider-key pools, model deployments, routing execution, streaming, cancellation, provider health and technical usage | conversations, memory, tools, approvals, agent identity or product behavior |
| **Alia** | conversations, agents, memory, tools, approvals, orchestration and assistant behavior | provider keys, provider adapters, model-deployment health or generic inference routing |
| **Oxy** | authentication, applications, scopes, customer credentials, authorization, catalogue policy, spend reservation, settlement and customer billing | provider execution or agent behavior |

Every model invocation is authorized at the Oxy edge. Kaana accepts only the
signed Oxy envelope; it is never a public credential issuer or a shortcut around
Oxy authorization.

## Exact deployment identity and order

`deploymentId` is the opaque identity of one exact Kaana deployment. It is never
derived from a provider slug, model name, display name, row position or database
order. Oxy resolves it from Kaana's signed descriptor surface and copies it into
the signed `authorizedRoutes` entry together with the exact revision-pinned model
reference, provider and complete region set.

Oxy performs the control-plane selection after all policy filters:

1. explicit routing-profile candidate `priority` first;
2. reviewed score descending within that priority;
3. exact `deploymentId` code units only as the equal-score tie-break.

Names, locale collation, insertion order and database return order never
participate. Kaana receives the already ordered authorized list, resolves every
ID against one inventory snapshot and attempts that exact order. Its runtime
health projection and breaker state may make an authorized attempt unavailable;
they never re-rank or authorize another destination.

Oxy fails closed before reservation and before calling Kaana if any otherwise
eligible deployment lacks an exact ID, price version or required score; if score
evidence is stale or belongs to another price version; or if the exact ID is
duplicated or collides with more than one approved mapping. Kaana independently
fails closed if the signed provider, model reference or region set does not match
the inventory entry for that ID.

An empty `regions` set means no execution/residency region is attested. It is not
an alias for global availability. Oxy excludes that deployment whenever the
effective policy has an allowed-region or denied-region control.

## Product request paths

```text
one-shot AI feature  -> Oxy inference edge -> Kaana -> upstream provider
agent/chat feature   -> Alia -> Oxy inference edge -> Kaana -> upstream provider
```

Use the first path when the product owns a bounded operation such as translate,
classify, summarize, rewrite or generate a smart reply. Use the second when the
operation is part of an assistant with conversation state, tools, memory,
approvals or agent identity.

The resulting product map is:

| Product surface | Canonical path |
|---|---|
| Mention assistant/chat | Mention -> Alia -> Oxy -> Kaana |
| Mention background translation, classification and moderation helpers | Mention -> Oxy -> Kaana |
| Inbox embedded assistant/chat | Inbox -> Alia -> Oxy -> Kaana |
| Inbox summary, rewrite and smart reply | Inbox -> Oxy -> Kaana |
| OxyOS assistant | OxyOS -> Alia -> Oxy -> Kaana |
| Homiio Sindi | Homiio -> Sindi as an Alia agent/bot -> Oxy -> Kaana |
| Clarity assistant | Clarity as an Alia agent/bot -> Oxy -> Kaana |

Sindi and Clarity therefore need Alia agent identities and bot accounts, not
new provider adapters. Their bot-account ownership and delegation must be
provisioned and verified before either path is called complete; this document
does not claim that deployment step has already happened.

## Provider keys: PostgreSQL plus KMS only

An upstream provider key has one durable home: Kaana's PostgreSQL
`provider_credentials` table. The stored value is KMS ciphertext bound by
encryption context to `provider + keyId`. Provider plaintext never belongs in an
environment variable, GitHub secret, task definition, inventory, command-line
argument or tracked file.

`DATABASE_URL` is the database connection credential and is not an upstream
provider key. Non-secret provider protocol and base-URL configuration may stay
in the task environment.

The one exception is the one-time migration reader: `kaana-credentials
import-ssm` may fetch an explicitly allow-listed legacy `SecureString` through
the AWS SDK, re-encrypt it immediately and emit no value. Cerebras is migrated
from the exact historical parameter documented in `operating.md`. An import is
not the retirement gate. First verify row metadata, publisher discovery and a
real Kaana request; only then delete the legacy parameter, its deployment
reference and the old service.

## External catalogues are leads, not sources

The local `itsfree.ai` checkout has no licence. It may identify a provider or
model family worth investigating, but Kaana copies none of its source, prose or
data. Every provider origin, protocol, model identity and availability claim is
re-derived from provider-owned documentation or an authenticated provider API,
then admitted through Kaana's onboarding gates.

## Database invariant

Kaana is PostgreSQL-only. Adding MongoDB, Mongoose or a Mongo connection string
is not a migration option or a fallback. Provider credentials, migration audit
records and any other durable Kaana state use PostgreSQL; the inference payload
itself is not persisted.
