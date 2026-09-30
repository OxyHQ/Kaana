# Identity and request routing

What Kaana is, what Alia is, and where Kaana's identity ends. The concepts an
app developer needs (exact model, power level, app default, and who owns what)
are in the [Oxy inference developer guide](https://github.com/OxyHQ/oxy/blob/main/docs/inference/README.md); which product feature
goes through Alia is decided in [Oxy's request-routing.md](https://github.com/OxyHQ/oxy/blob/main/docs/inference/request-routing.md).

## One name and one origin

The inference data plane is **Kaana**. Its repository is `OxyHQ/Kaana`, its
configuration uses `KAANA_*`, and its only canonical signed data-plane origin is
[`https://kaana.ai`](https://kaana.ai).

The former inference-service name is not an internal compatibility identity.
Historical task, SSM and environment names may appear only in the one-time
credential importer or the retirement runbook. They must not appear in a new
route, hostname, package, task, model alias or public instruction.
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
| **Oxy** | authentication, applications, scopes, Oxy login/API credentials, provider-connection metadata, authorization, catalogue policy, spend reservation, settlement and customer billing | upstream provider-secret custody, provider execution or agent behavior |

Every model invocation is authorized at the Oxy edge. Kaana accepts only the
signed Oxy envelope; it is never a public credential issuer or a shortcut around
Oxy authorization. Developer access is managed in **Oxy Console**, the single
console for every Oxy API: an application and its API keys are registered there
whether they call Kaana, Alia or Mention. Alia, in turn, is the assistant
product — the ChatGPT of Oxy — with a product API of its own that other Oxy apps
call; a request to that API is authorized by Oxy the same way and reaches Kaana
only through the Oxy edge.

## Exact deployment identity and order

`deploymentId` is the opaque identity of one exact Kaana deployment. It is never
derived from a provider slug, model name, display name, row position or database
order. Oxy copies it into the signed `authorizedRoutes` entry together with the
exact revision-pinned model reference, provider and complete region set, in an
order Oxy decides ([Oxy's routing.md](https://github.com/OxyHQ/oxy/blob/main/docs/inference/routing.md#ranking-after-qualification)). Kaana resolves every ID against one
inventory snapshot, fails closed if the signed provider, model reference or
region set does not match, and attempts the list in exactly that order. Its
health projection and breakers may make an authorized attempt unavailable; they
never re-rank the list or authorize another destination. An empty `regions` set
means no attested region, not global availability.

## Product request paths

```text
app one-shot AI -> Oxy inference edge -> Kaana -> upstream provider
app agent/chat  -> Alia -> Oxy inference edge -> Kaana -> upstream provider
```

The per-product map (Mention, Inbox, OxyOS, Sindi, Clarity) is kept in one
place, [Oxy's request-routing.md](https://github.com/OxyHQ/oxy/blob/main/docs/inference/request-routing.md#choose-the-path-by-product-behavior).
Whichever path a feature takes, only Kaana retries and fails over, along the
routes Oxy signed; apps and Alia do not.

Customer BYOK is not enabled merely because Kaana can decrypt an exact signed
generation; its launch gates are Oxy's ([byok.md](https://github.com/OxyHQ/oxy/blob/main/docs/inference/byok.md)).
Kaana receives no customer price and cannot approve that commercial decision.

## Provider keys: PostgreSQL plus KMS only

An upstream provider key has one durable home: Kaana's PostgreSQL
`provider_credentials` table. The stored value is KMS ciphertext bound by
encryption context to `provider + keyId`. Provider plaintext never belongs in an
environment variable, GitHub secret, task definition, inventory, command-line
argument or tracked file.

Credential identity is separate from routing identity. Oxy signs an exact
`deploymentId`; Kaana resolves that deployment's exact `(provider, keyId)` row.
It never substitutes a provider display name, pool position, insertion order or
the first enabled credential for either ID.

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
