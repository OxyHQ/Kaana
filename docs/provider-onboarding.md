# Provider onboarding

The verified wire contract, catalogue boundary and current Kaana implementation
state for the next provider cohort. This is an implementation guide, not a
claim that an account currently holds access: availability is accepted only
from the provider's authenticated account catalogue, and every provider key
lives as KMS ciphertext in Kaana's PostgreSQL credential store.

The original cohort below was checked on 2026-09-01. The Alibaba and Cloudflare
additions were re-derived on 2026-09-02 from provider-owned API, lifecycle and
model documentation. Nothing in this document or the corresponding attribution
allow-list is sourced from a third-party catalogue.

## The three separate gates

Calling an API, discovering a deployment and publishing an immutable model
reference are different decisions:

1. **Serving** requires a protocol adapter that can express the request and
   account for the provider's real response, stream, usage and error shapes.
2. **Discovery** requires an authenticated account model-list endpoint. An API
   being OpenAI-compatible for Chat Completions does not prove that it publishes
   `GET /models`.
3. **Publication** requires an upstream id that still names the same model
   tomorrow plus an explicit publisher attribution. A `latest` alias, router,
   preview or delivery tier cannot become an immutable Kaana reference merely
   because discovery returned it.

The shared `openaicompat` adapter constructs `POST {base}/chat/completions`.
The publisher has separate discovery profiles because model-list semantics are
provider facts, not adapter facts.

## Verified matrix

Every provider in this cohort uses `Authorization: Bearer <provider-key>` for
the endpoints below. Examples in provider documentation often source that value
from an environment variable; Kaana does not. It decrypts the selected database
credential only at send time.

| Provider | OpenAI-compatible base URL | Chat endpoint | Account model discovery | Model identity status | Current Kaana implementation |
|---|---|---|---|---|---|
| Mistral | `https://api.mistral.ai/v1` | `POST /chat/completions` | `GET /models`; response includes `capabilities.completion_chat` | Fixed GA ids are available; `*-latest` and major aliases move; `labs-*` may update silently | Built-in `openaicompat` serving; publisher filters for chat capability; five fixed ids are attributed. No live credential conformance has been recorded. |
| DeepSeek | `https://api.deepseek.com` | `POST /chat/completions` | `GET /models`, OpenAI list shape | Current direct ids are moving aliases; vision id is experimental | Built-in `openaicompat` serving and generic discovery. Direct attribution is deliberately absent, so discovery cannot emit a direct DeepSeek deployment yet. |
| SambaNova | `https://api.sambanova.ai/v1` | `POST /chat/completions` | `GET /models`; includes context, max output and pricing metadata | Four production ids are allowed; `DeepSeek-V3.2` is preview | Built-in `openaicompat` serving and generic discovery; four production ids are attributed. Publisher currently consumes only ids, not SambaNova's richer metadata. No live credential conformance has been recorded. |
| Alibaba Model Studio | Workspace- and region-scoped; for Singapore, `https://{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1` | `POST /chat/completions` | Authenticated native `GET /api/v1/models`, paginated and filterable by capability/support | Four dated Qwen snapshots are allowed; moving family ids and previews remain absent | Built-in protocol and endpoint identity with explicit base; native authenticated discovery; exact snapshot attribution. No live credential conformance has been recorded. |
| Cloudflare Workers AI | `https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1` | `POST /chat/completions` | Separate authenticated `GET /accounts/{account_id}/ai/models/search`; official result rows are currently untyped | Catalogue ids and lifecycle are provider-managed; no id is attributed by this change | Built-in protocol and endpoint identity with explicit base; serving only. Discovery fails closed until Cloudflare publishes a stable row schema. |
| SiliconFlow | `https://api.siliconflow.cn/v1` | `POST /chat/completions` | `GET /models?type=text&sub_type=chat` | Versioned upstream ids exist; `Pro/` is a delivery/payment tier, not different weights | Built-in `openaicompat` serving; publisher applies both discovery filters; five fixed chat ids are attributed. No live credential conformance has been recorded. |
| AI21 | `https://api.ai21.com/studio/v1` | `POST /chat/completions` | No `GET /models` is documented in the current API reference | Two dated Jamba snapshots are fixed; shorter names are aliases | Built-in `openaicompat` serving configuration. Discovery is `not_available`, so the publisher refuses AI21. No attribution or static-catalog path exists yet. |
| Nebius Token Factory | `https://api.tokenfactory.nebius.com/v1` | `POST /chat/completions` | Authenticated `GET /models?verbose=true`, OpenAI list plus rich model metadata | The documented `-fast` flavour may not mint a new model identity; any other id still needs exact attribution | Built-in `openaicompat` serving and provider-specific authenticated discovery. Two fixed Meta ids from official examples are attributed; they remain absent until a verbose account list returns them as base deployments. |
| Nscale | `https://inference.api.nscale.com/v1` | `POST /chat/completions` | Authenticated, organization-scoped `GET /models`; the catalogue mixes chat, vision, embeddings and image generation | Versioned ids exist, but task capability still needs review | Built-in `openaicompat` serving and generic authenticated discovery. One fixed Meta chat id from the official guide is attributed; every other row remains dropped. |
| Chutes | `https://llm.chutes.ai/v1` | `POST /chat/completions` | The documented `/models` catalogue is public, not an account entitlement list | TEE suffixes are deployment facts; saved aliases and routing strategies move | Built-in serving only. Discovery is `not_available` until account access, routing identity and streamed usage have a provider-specific control. |
| OVHcloud AI Endpoints | `https://oai.endpoints.kepler.ai.cloud.ovh.net/v1` | `POST /chat/completions` | Public catalogue at a separate `catalog.endpoints.ai.ovh.net` origin | Availability and decommission state are mutable deployment facts | Built-in serving only. Discovery is `not_available`; Kaana will not send a provider credential to the public catalogue or mistake catalogue presence for account access. |

"Built in" above means that the current tree can construct the shared adapter
with the documented base URL. It does **not** mean that provider-specific error
types, optional fields, streaming edge cases or usage semantics have passed a
real provider call. Before production enablement, each provider still needs a
scrubbed conformance fixture captured from its own API and registered in the
adapter conformance suite.

## Mistral

Mistral documents the base URL, bearer authentication and Chat Completions API,
and publishes an authenticated `GET /v1/models`. The list includes a
`completion_chat` capability, so the publisher must not treat every returned id
as a chat deployment.[^mistral-api][^mistral-models-api]

Fixed, chat-capable candidates currently allowed by Kaana:

- `mistral-medium-3-5`
- `mistral-small-2603`
- `mistral-large-2512`
- `ministral-8b-2512`
- `ministral-14b-2512`

Mistral's lifecycle documentation distinguishes a fixed major/minor id from
`-latest` and major aliases, both of which automatically move. Labs releases
are experimental, may receive silent updates, have a shorter retirement window
and are not production candidates.[^mistral-lifecycle]

Kaana status:

- `providerconfig.Known["mistral"]` selects `openaicompat`, the official base
  URL and the `mistral_models` discovery profile.
- Discovery keeps only rows whose `capabilities.completion_chat` is true.
- `configs/model-attribution.json` allows only the five fixed ids above.
- Free mode is an account plan with included monthly usage, not a permanent
  property of a model deployment. It must not become `cost = 0` in inventory.
  Labs being free is not enough to overcome their mutable lifecycle.[^mistral-free]

## DeepSeek

DeepSeek documents `https://api.deepseek.com` as its OpenAI-format base, bearer
authentication, `POST /chat/completions` and an OpenAI-shaped authenticated
`GET /models`.[^deepseek-quickstart][^deepseek-models]

Current callable ids:

- `deepseek-v4-flash` — moving alias, currently V4-Flash-0731
- `deepseek-v4-pro` — moving alias, currently V4-Pro-0813
- `deepseek-v4-flash-vision-exp` — experimental vision deployment

The first two names are explicitly the stable calling method for whichever
current V4 versions DeepSeek assigns to them. The official direct API
documentation does not declare the dated version labels as independently
callable model ids. None of the three therefore qualifies as an immutable
Kaana model reference.[^deepseek-pricing]

Kaana status:

- Serving and the OpenAI model-list shape are configured.
- There is intentionally no direct `deepseek` attribution block. The publisher
  drops the discovered ids instead of minting an immutable reference around a
  moving alias or preview.
- Provider-specific thinking, reasoning fields and the experimental Files/Vision
  path are outside the shared adapter's verified baseline. They require a real
  DeepSeek conformance fixture before Kaana can claim them.
- The API is metered. DeepSeek may consume granted balance before topped-up
  balance, but documents no recurring free allocation that Kaana can treat as
  provider-key class or model cost.[^deepseek-pricing]

## SambaNova

SambaCloud publishes `https://api.sambanova.ai/v1` for OpenAI clients and the
full Chat Completions URL. Its authenticated model-list response includes
`context_length`, `max_completion_tokens` and provider pricing in addition to
the OpenAI list fields.[^sambanova-urls][^sambanova-models-api]

Fixed production candidates currently allowed by Kaana:

- `MiniMax-M2.7`
- `DeepSeek-V3.1`
- `Meta-Llama-3.3-70B-Instruct`
- `gpt-oss-120b`

`DeepSeek-V3.2` is documented as Preview, for evaluation rather than production,
and is excluded. SambaNova says preview capacity is limited and the model may be
removed at short notice. Its production deprecation notice is also short — two
to three weeks — so authenticated discovery must remain the deployment truth,
not the checked-in allow-list.[^sambanova-catalogue][^sambanova-deprecations]

Kaana status:

- Serving and generic OpenAI list discovery are configured.
- The four production ids are attributed; the preview id is not.
- Discovery currently discards the context, output-limit and pricing fields.
  Those are candidates for a provider metadata observation, but pricing remains
  operator-only `internal/providercost`, never inventory or the customer
  contract.
- The no-payment-method Free Tier is an account tier with low per-model limits,
  not a permanent model property. Current documentation lists 20 RPM, 20 RPD
  and 200,000 tokens/day for the free entries shown.[^sambanova-limits]

## Alibaba Cloud Model Studio

Alibaba's compatible endpoint is not one global origin. It is bound to a
workspace and region. The recommended Singapore base is:

```text
https://{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1
```

Virginia, Beijing, Hong Kong, Frankfurt and Tokyo use different regional
origins. The API key, workspace and base URL must belong to the same region.
Kaana must store that association as provider configuration and refuse a
placeholder or mismatched origin.[^alibaba-chat]

Only a **general-purpose pay-as-you-go Model Studio key** is admissible for the
Kaana backend. Alibaba explicitly prohibits Token Plan and Coding Plan keys in
automation scripts, custom application backends and non-interactive batch
calls; misuse can suspend the subscription or revoke the key.[^alibaba-token-plan]

Fixed snapshot candidates documented by Alibaba include:

- `qwen3.7-plus-2026-05-26`
- `qwen3.7-flash-2026-07-15`
- `qwen3.6-plus-2026-04-02`
- `qwen3.6-flash-2026-04-16`

The family ids without a date are not substitutes for these snapshots. Several
snapshot families are already listed as legacy even though their fixed ids
remain documented, so the attribution allow-list still needs lifecycle review
instead of treating a once-seen id as permanently deployable.[^alibaba-catalogue][^alibaba-lifecycle]

Alibaba now documents a separate authenticated native model-list API at
`GET /api/v1/models`. It accepts `capabilities=TG`, `supports=inference`,
`page_no` and `page_size`, and returns the exact callable id as
`output.models[].model` plus input/output modality metadata. It is not the
OpenAI `GET /models` shape and is not located below `/compatible-mode/v1`, so a
generic compatibility probe would still be wrong.[^alibaba-model-list]

Kaana status:

- `providerconfig.Known["alibaba"]` supplies the reviewed OpenAI-compatible
  protocol and native discovery profile but deliberately no root. Both serving
  and publisher require the exact non-secret workspace/region base URL.
- Endpoint validation accepts only Alibaba's documented compatibility origins
  and exact `/compatible-mode/v1` path. The workspace id remains an opaque URL
  segment; Kaana never parses it or uses ordering to infer identity.
- Discovery maps the serving origin to the provider-documented regional
  catalogue origin, requests only text-generation inference rows, follows every
  page, requires a stable declared total and rejects duplicate or normalized
  ids. A legacy/non-workspace base that does not identify its documented
  catalogue workspace is refused rather than guessed.
- Only exact `response_modality=["Text"]` rows proceed to attribution, and only
  the four dated snapshots above are attributed. Undated families, previews and
  every other catalogue row are dropped and named.
- The shared adapter passes Kaana's synthetic OpenAI-wire conformance suite
  under the `alibaba` slug. A scrubbed real-account capture of invalid-key,
  non-stream, SSE, usage, tools and quota errors is still required before
  production enablement.
- The provider API key remains exclusively a KMS ciphertext row in Kaana's
  PostgreSQL credential store. The workspace/region URL is non-secret process
  configuration and never substitutes for the key.
- New-user free quota is regional, model-specific and time-limited; it can roll
  into paid usage. It must be recorded as expiring account evidence, never as
  a fixed model price. Alibaba's documentation announces a 90-day rule for new
  activations from 2026-09-08 and should be rechecked after that date.[^alibaba-free]

## Cloudflare Workers AI

Cloudflare documents an OpenAI-compatible Chat Completions base whose path
contains the account id:

```text
https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1
```

The API token uses bearer authentication. In Kaana it remains only in the
encrypted PostgreSQL credential store; the account-scoped URL is non-secret
configuration. Endpoint validation requires the exact Cloudflare host and path
shape and treats the account id as one opaque segment.[^cloudflare-openai]

Cloudflare separately documents an authenticated model-search endpoint at
`GET /accounts/{account_id}/ai/models/search`, including task, experimental and
deprecation filters. Its currently published API contract types both default
and marketplace result rows as unknown rather than defining the id and task
fields Kaana would have to trust. The compatibility root also does not thereby
gain an OpenAI-shaped `/models` endpoint.[^cloudflare-model-search]

Kaana status:

- `providerconfig.Known["cloudflare"]` supplies the reviewed shared protocol
  and exact account-scoped endpoint identity, but no default root.
- The shared adapter passes the synthetic OpenAI-wire conformance suite under
  the `cloudflare` slug. A scrubbed real-account wire capture remains required.
- Discovery is deliberately `not_available`, there are no Cloudflare
  attribution rows, and publisher startup refuses the slug. Parsing an
  undocumented response shape would turn a provider change into silent model
  identity drift.
- No free-tier, price or regional-residency claim is made. Those are account and
  deployment facts that require separately timestamped evidence.

## SiliconFlow

SiliconFlow documents the China service at `https://api.siliconflow.cn/v1`,
bearer authentication, Chat Completions and an OpenAI-shaped model-list. The
list covers text, image, audio and video unless filtered; a chat publisher must
send both `type=text` and `sub_type=chat`.[^silicon-chat][^silicon-models]

Fixed chat candidates currently allowed by Kaana:

- `Pro/zai-org/GLM-5`
- `Pro/zai-org/GLM-4.7`
- `deepseek-ai/DeepSeek-V3.2`
- `Qwen/Qwen3.5-397B-A17B`
- `deepseek-ai/DeepSeek-V3.1-Terminus`

For models offered in both forms, SiliconFlow describes the unprefixed id as
the free delivery and `Pro/` as the paid delivery. That prefix changes the
upstream route and rate-limit tier, not the underlying weights. A canonical
model identity must therefore omit `Pro/`, while the deployment preserves the
exact upstream id.[^silicon-limits]

Kaana status:

- Serving uses the built-in shared adapter.
- The `siliconflow_models` discovery profile applies the two required filters.
- Attribution maps the five exact upstream ids to model identities and does not
  add both free and Pro variants as two model lines.
- No static rule may classify every unprefixed id as free: SiliconFlow only
  documents that naming convention for models that have a free/paid pair, and
  model availability and pricing can change.[^silicon-limits][^silicon-releases]
- Thinking controls such as `enable_thinking`, `thinking_budget` and `min_p`
  are provider extensions outside the shared adapter's verified baseline.

## AI21

AI21 requires bearer authentication and exposes Jamba Chat Completions at
`POST https://api.ai21.com/studio/v1/chat/completions`.[^ai21-auth][^ai21-chat]
The current API reference does not document an account `GET /models`. Endpoint
naming similarity is not sufficient evidence to invent one.

Fixed candidates:

- `jamba-large-1.7-2025-07`
- `jamba-mini-2-2026-01`

The shorter `jamba-large`, `jamba-mini`, `jamba-large-1.7` and `jamba-mini-2`
names point to those dated snapshots today, but are aliases and cannot back an
immutable reference.[^ai21-models]

Kaana status:

- The serving process has an `openaicompat` default and official API root for
  `ai21`; this is configuration reuse, not proof of complete OpenAI parity.
- Discovery is explicitly `not_available`, and publisher startup refuses AI21
  rather than trying `/models`.
- No AI21 attribution entries exist because there is no verified account-list
  path connecting the documented catalogue to this credential's access.
- Onboarding needs a reviewed static-catalogue control, followed by a real
  non-stream, SSE, usage, error and tool-call conformance capture.
- New accounts receive a USD 10 credit for three months, after which billing is
  required. That trial is expiring account state, not a free model tier.[^ai21-pricing]

## Live OpenRouter, Groq and xAI catalogue delta

The publisher task's authenticated 2026-09-01 run exposed a useful trap: an
upstream model-list delta is not an attribution to-do list. The public
`itsfree.ai` checkout helped identify families worth checking, but it has no
licence; Kaana reused none of its source, prose or data. Every decision below
was re-derived from the live publisher result and provider-owned metadata.

OpenRouter's current catalogue gives each of these ids text output and either a
fixed canonical slug or the same canonical slug as an already-attributed free
route, so they are now attributed:[^openrouter-models]

- `ibm-granite/granite-4.2-8b`
- `inclusionai/ling-3.0-flash-fin:free`
- `minimax/minimax-m2.7:free`
- `minimax/minimax-m3:free`
- `mistralai/devstral-2512`
- `qwen/qwen3.8-flash`
- `tencent/hy-mt2-7b`
- `thinkingmachines/inkling:free`
- `thinkingmachines/inkling-small:free`
- `z-ai/glm-5.3-flash`

They are still deployments only when authenticated discovery returns the exact
id. Their catalogues do not report execution or residency regions, so this
change deliberately declares none.

The other warnings remain exclusions:

- OpenRouter `:batch` ids are delivery routes, `~...latest` and
  `openai/gpt-chat-latest` move, and `openrouter/*` ids are routers rather than
  one set of weights.
- Preview-labelled ids are not promoted merely because their response shape is
  compatible.
- Groq's Compound ids are systems that choose among models and tools; Orpheus
  and Whisper use speech endpoints and output types the Chat Completions
  adapter cannot represent.[^groq-systems][^groq-speech][^groq-transcription]
- Groq lists `qwen/qwen3.8-27b` as Preview, so the direct Groq route remains
  unattributed even though the fixed model line is available elsewhere.[^groq-qwen]
- xAI's Imagine ids belong to image- or asynchronous video-generation APIs,
  not the text stream Kaana currently normalizes.[^xai-image-models][^xai-video]

## Existing Alia provider surface migrated into Kaana

The old in-process Alia provider tree also named Google Gemini, Together,
Cohere, Fireworks, Hyperbolic and DigitalOcean. Their official compatibility
origins are now built into Kaana, so moving those credentials into Kaana's
encrypted PostgreSQL store does not require carrying an endpoint in Alia or in
a secret environment variable.[^google-openai][^together-openai][^cohere-openai][^fireworks-openai][^hyperbolic-openai][^digitalocean-openai]

This is serving configuration, not automatic publication:

- Together and DigitalOcean document OpenAI-compatible model listing at their
  compatibility roots, so Kaana can perform authenticated discovery. No model
  is emitted until an exact, immutable upstream id has an explicit publisher
  attribution.
- Google's documented model list uses the native Gemini shape and path, not the
  compatibility root's OpenAI list shape. Cohere's list is likewise native, and
  Fireworks and Hyperbolic do not document the generic account-list contract
  Kaana's publisher consumes. Those four are therefore marked
  `not_available` for discovery instead of trying a plausible `/models` URL.
- Cloudflare Workers AI and Alibaba Model Studio now carry built-in protocol and
  endpoint-identity rules, while their account/workspace-scoped roots remain
  explicit through `KAANA_PROVIDER_<SLUG>_BASE_URL`. Both API tokens remain only
  in Kaana's encrypted credential database. Alibaba has a separately verified
  native discovery profile; Cloudflare remains serving-only because its current
  official model-search rows are untyped.[^cloudflare-openai][^cloudflare-model-search][^alibaba-model-list]
- Perplexity's current Sonar endpoint is `/v1/sonar`, not the
  `/chat/completions` path the shared adapter constructs, so it is not falsely
  advertised as implemented by this cohort.

## Additional direct providers recovered from the external catalogue

The review also surfaced three provider-owned, direct Chat Completions origins
that the first migration inventory did not contain:

- NVIDIA hosted NIM: `https://integrate.api.nvidia.com/v1`. NVIDIA documents
  the hosted `POST /chat/completions` endpoint as OpenAI-compatible. Its free
  hosted access is for evaluation, so this built-in is not permission to publish
  a public production deployment.[^nvidia-hosted-chat]
- ModelScope API Inference: `https://api-inference.modelscope.cn/v1`.
  ModelScope's own model pages invoke it through the OpenAI client. The service
  documentation explicitly describes it as a free, non-commercial experience
  without production SLA, so Oxy's commercial approval gate must keep it out of
  public resale.[^modelscope-qwen][^modelscope-limits]
- Zhipu/BigModel (`zai`): `https://open.bigmodel.cn/api/paas/v4`. The official
  API reference documents bearer-authenticated `POST /chat/completions`,
  including streaming and tool-capable responses.[^zai-chat]

All three use the shared serving adapter and are marked `not_available` for
discovery. A provider documenting one Chat Completions URL does not establish
an authenticated account-wide `/models` contract, and Kaana never probes a
plausible URL as if it were evidence. They therefore require a controlled
static catalogue or a later provider-owned discovery source before publication.

Ollama Cloud remains outside this built-in set. Ollama officially documents its
remote native API at `https://ollama.com/api` and the OpenAI-compatible surface
for local Ollama, but does not currently document a remote OpenAI-compatible
base that Kaana can pin without inference.[^ollama-cloud][^ollama-openai]

## Second direct-provider cohort

The same external-catalogue review identified four additional provider-owned
origins. ItsFree was a search lead only: its Nebius address was the retired AI
Studio hostname, while Nebius now documents Token Factory at
`https://api.tokenfactory.nebius.com/v1`. Kaana therefore carries no provider
fact merely because the external repository lists it.[^nebius-api]

Nebius and Nscale both document bearer-authenticated Chat Completions and a
model list at the same root; Nscale explicitly scopes its OpenAI-shaped list to
the authenticated organization. Nebius discovery requests `verbose=true` and
discards the documented `-fast` delivery flavour before attribution. Any other
unreviewed alias is dropped by the exact attribution allow-list. Nscale uses the
generic OpenAI list profile.
The official examples provide two fixed Meta ids for Nebius and one for Nscale,
so those exact ids are attributed to canonical model lines already established
by existing routes. They still create no deployment unless the authenticated
account list returns the exact id. A first real capture must retain Nebius's
delivery-flavour evidence and review Nscale task capability before
the allow-list grows.[^nebius-models][^nebius-flavours][^nscale-chat][^nscale-models]

Chutes documents an OpenAI-compatible streaming endpoint, bearer keys and a
live `/models` catalogue. Its platform also exposes saved aliases and routing
strategies, and model capabilities depend on the selected backend. The shared
adapter can therefore be configured from the built-in origin, while publisher
startup refuses it until Kaana has a provider-specific identity and conformance
control. `-TEE` describes Chutes's attested delivery, not a second set of model
weights.[^chutes-start][^chutes-tools]

OVHcloud documents Chat Completions, streaming tool-call deltas and bearer
authentication. Its official discovery source is instead a public catalogue on
a different origin, explicitly separate from inference and subject to breaking
changes. That catalogue reports `available`, category, modalities and lifecycle
metadata, but not what one Kaana credential is entitled to invoke. OVHcloud is
therefore built in for serving and deliberately `not_available` to the current
account publisher.[^ovh-tools][^ovh-catalogue]

Hugging Face Inference Providers, Kilo AI Gateway, LLM7 and OpenCode Zen are not
added as direct built-ins. Their documented surfaces route among downstream
providers or protocols; moving selectors such as `:fastest`, `:cheapest`,
`default`, `kilo-auto/*` and mixed-protocol model entries do not meet Kaana's
single-provider deployment identity. An operator can still declare a reviewed
HTTPS origin explicitly, but publication needs a router-aware contract first.
GLHF is also excluded because no current provider-owned wire contract could be
verified.[^hf-router][^kilo-gateway][^llm7-models][^opencode-zen]

## 2026-09-03 external radar disposition

The unlicensed external catalogue was used only to discover provider names.
No endpoint, model id, price, quota or descriptive text was imported from it.
Each name was re-evaluated against a provider-owned source, and a name that did
not clear every serving, identity and cost gate below produced no configuration
or attribution change.

The direct providers surfaced by that radar and supported by official evidence
were already present in `providerconfig.Known`: Google AI Studio, Groq, NVIDIA
hosted NIM, Cerebras, Cloudflare Workers AI, Mistral, Cohere, DeepSeek,
ModelScope, Zhipu/BigModel, SambaNova, OVHcloud, Alibaba Model Studio,
SiliconFlow, AI21, Nscale and Nebius Token Factory. This review therefore adds
no duplicate slug and no copied model row. OpenRouter was also already present,
under its existing explicit gateway identity and endpoint binding; this review
does not widen that exception to a second gateway.

The remaining candidates fail closed for concrete, provider-owned reasons:

| Candidate | Provider-owned evidence | Why Kaana does not add it |
|---|---|---|
| Requesty | Its documentation calls the origin a router and requires provider-prefixed model ids.[^requesty-router][^requesty-openai] | The Requesty key identifies a gateway while the actual serving provider is selected downstream. Kaana cannot bind one deployment to one provider or reconcile the direct provider cost from that request. |
| Vercel AI Gateway | Vercel documents automatic provider selection, ordering and fallback, and says provider availability and price can differ for one model.[^vercel-routing] | A model id alone does not fix the serving provider. Adding the gateway as if it were a direct provider would make provider identity and upstream cost depend on a runtime routing decision. |
| Hugging Face Inference Providers | Hugging Face documents `:fastest`, `:cheapest`, `:preferred` and provider suffixes, with automatic provider selection as the default.[^hf-router] | The default route is deliberately dynamic. A future router-aware contract could carry the selected downstream provider, but today's direct-provider deployment cannot. |
| Kilo AI Gateway | Kilo documents one endpoint over many providers plus `kilo-auto/*` virtual models whose underlying model can change.[^kilo-gateway] | Both downstream provider and, for auto ids, model identity can move. Neither is an immutable Kaana deployment. |
| OpenCode Zen | OpenCode describes Zen as an AI gateway and publishes a mixed endpoint table, including Responses-only entries.[^opencode-zen] | Gateway identity is not downstream provider identity, and the mixed wire is not the one shared Chat Completions adapter contract. |
| Aion Labs | The API reference fixes a Chat Completions wire and publishes prices, but its model list is public rather than an authenticated entitlement list, and Aion's terms define the service as routing requests to upstream AI model providers and third-party hosting infrastructure.[^aion-api][^aion-terms] | The response contract does not expose a stable downstream provider/cost identity that Kaana can bind and reconcile. An Aion-branded model name is not proof of one direct deployment. |
| Agnes AI | The provider-owned public surface mixes Agnes, OpenAI, Google and other publishers, while no stable provider-owned Chat Completions and model-list contract was found.[^agnes-public] | A marketing catalogue cannot substitute for authenticated wire, lifecycle and billing semantics. |
| GLHF | No current provider-owned API contract was found that fixes auth, streaming usage, model identity and price. | The endpoint lead alone is insufficient evidence. |
| AMD Radeon Cloud | The provider-owned Token Factory page is interactive and did not publish a reviewable API schema, authentication contract, model lifecycle or price/quota semantics.[^amd-token-factory] | An endpoint inferred from a portal example is not a deterministic production contract. No AMD slug or model attribution is added. |
| Ollama Cloud | Ollama documents the remote service at `https://ollama.com/api`, with bearer authentication and the native `/api/chat` and `/api/tags` wire. Its OpenAI-compatibility documentation demonstrates the local `http://localhost:11434/v1` server instead.[^ollama-cloud][^ollama-openai] | Kaana contains only OpenAI Chat Completions and Anthropic Messages adapters. Pointing the OpenAI adapter at an undocumented remote `/v1` path would be guesswork; adding Ollama's native wire requires a separately reviewed adapter. |
| vLLM, MLX, llamafile, local Ollama, LM Studio, llama.cpp and Jan | Their official projects describe operator-run local or self-hosted servers.[^vllm-serve][^mlx-serve][^llamafile][^ollama-openai][^lmstudio-server][^llamacpp-server][^jan-server] | They have no provider-owned global HTTPS identity, account catalogue or price. Kaana's arbitrary explicit HTTPS provider configuration remains the correct escape hatch for a separately operated deployment; none becomes a global built-in. |

Requesty, Vercel, Hugging Face, Kilo, OpenCode, Aion Labs and the other
excluded candidates also receive no `configs/model-attribution.json` block.
That absence is intentional: a public model list, promotional free tier or
display name cannot establish immutable weights or the cost paid to the direct
provider. `internal/providerconfig` and the checked-in attribution tests pin
this fail-closed disposition.

## Completion criteria

A provider is production-onboarded only when all applicable checks below are
green:

- its official HTTPS base and auth scheme are represented without storing a
  provider key in environment, argv, manifests or tracked files;
- a real invalid key is classified and redacted, and redirects are refused;
- non-streamed and SSE responses pass `internal/provider/conformance`, including
  tool calls, usage partitioning, cancellation and mid-stream failure;
- discovery is authenticated and account-scoped, with chat-only filtering;
- every published upstream id is fixed, callable and explicitly attributed;
- moving aliases, routers, previews and delivery-only variants are rejected or
  mapped only as non-reference deployment metadata;
- 401, throttle 429, exhausted quota/balance and unavailable model responses
  are classified from the provider's own error type rather than HTTP status
  alone;
- free/trial claims carry `observedAt`, account/region scope and `expiresAt`
  when applicable; absence of pricing remains unknown, never zero;
- a disappeared deployment is withdrawn without changing the revision of any
  surviving pinned reference.

## Official sources

[^mistral-api]: [Mistral API — Chat](https://docs.mistral.ai/api)
[^mistral-models-api]: [Mistral API — Models endpoints](https://docs.mistral.ai/api/endpoint/models)
[^mistral-lifecycle]: [Mistral model lifecycle and alias convention](https://docs.mistral.ai/inference/model-lifecycle)
[^mistral-free]: [Mistral Studio activation and Free mode](https://docs.mistral.ai/getting-started/quickstarts/studio/activate-and-generate-api-key)
[^deepseek-quickstart]: [DeepSeek — Your First API Call](https://api-docs.deepseek.com/)
[^deepseek-models]: [DeepSeek — Lists Models](https://api-docs.deepseek.com/api/list-models/)
[^deepseek-pricing]: [DeepSeek — Models and Pricing](https://api-docs.deepseek.com/quick_start/pricing/)
[^sambanova-urls]: [SambaNova — API keys and URLs](https://docs.sambanova.ai/docs/en/get-started/api-keys-urls)
[^sambanova-models-api]: [SambaNova — available model list metadata](https://docs.sambanova.ai/docs/api-reference/models/get-environments-available-model-list-metadata)
[^sambanova-catalogue]: [SambaCloud models](https://docs.sambanova.ai/docs/en/models/sambacloud-models)
[^sambanova-deprecations]: [SambaNova model deprecations](https://docs.sambanova.ai/docs/en/models/deprecations)
[^sambanova-limits]: [SambaCloud rate-limit tiers](https://docs.sambanova.ai/docs/en/models/rate-limits)
[^alibaba-chat]: [Alibaba Model Studio — OpenAI-compatible Chat](https://www.alibabacloud.com/help/en/model-studio/compatibility-of-openai-with-dashscope)
[^alibaba-token-plan]: [Alibaba Model Studio Token Plan usage policy](https://docs.modelstudio.console.alibabacloud.com/en/model-studio/token-plan-personal-overview)
[^alibaba-catalogue]: [Alibaba Model Studio text-generation models](https://www.alibabacloud.com/help/en/model-studio/text-generation-model)
[^alibaba-model-list]: [Alibaba Model Studio — authenticated model list](https://docs.modelstudio.console.alibabacloud.com/en/model-studio/list-models)
[^alibaba-lifecycle]: [Alibaba Model Studio — model decommissioning policy](https://www.alibabacloud.com/help/en/model-studio/model-depreciation)
[^alibaba-free]: [Alibaba Model Studio new-user free quota](https://www.alibabacloud.com/help/en/model-studio/new-free-quota)
[^silicon-chat]: [SiliconFlow — Chat Completions](https://docs.siliconflow.cn/en/api-reference/chat-completions/chat-completions)
[^silicon-models]: [SiliconFlow — List Models](https://docs.siliconflow.cn/en/api-reference/models/get-model-list)
[^silicon-limits]: [SiliconFlow rate limits and free/Pro convention](https://docs.siliconflow.cn/en/userguide/rate-limits/rate-limit-and-upgradation)
[^silicon-releases]: [SiliconFlow release and service-adjustment notes](https://docs.siliconflow.cn/en/release-notes/overview)
[^ai21-auth]: [AI21 authentication](https://docs.ai21.com/reference/authentication)
[^ai21-chat]: [AI21 Jamba Chat request](https://docs.ai21.com/reference/jamba-1-6-api-ref)
[^ai21-models]: [AI21 Jamba models and API versioning](https://docs.ai21.com/docs/jamba-foundation-models)
[^ai21-pricing]: [AI21 pricing and introductory credit](https://docs.ai21.com/docs/usage-cost)
[^openrouter-models]: [OpenRouter model catalogue API](https://openrouter.ai/api/v1/models)
[^groq-systems]: [Groq Compound systems](https://console.groq.com/docs/compound/systems)
[^groq-speech]: [Groq Orpheus text-to-speech](https://console.groq.com/docs/text-to-speech/orpheus)
[^groq-transcription]: [Groq API reference — audio transcription](https://console.groq.com/docs/api-reference)
[^groq-qwen]: [Groq Qwen 3.8 27B model status](https://console.groq.com/docs/model/qwen/qwen3.8-27b)
[^xai-image-models]: [xAI image-generation model API](https://docs.x.ai/developers/rest-api-reference/inference/models)
[^xai-video]: [xAI Grok Imagine video](https://docs.x.ai/developers/models/grok-imagine-video)
[^google-openai]: [Gemini API OpenAI compatibility](https://ai.google.dev/gemini-api/docs/openai)
[^together-openai]: [Together OpenAI compatibility](https://docs.together.ai/docs/inference/openai-compatibility)
[^cohere-openai]: [Cohere Compatibility API](https://docs.cohere.com/docs/compatibility-api)
[^fireworks-openai]: [Fireworks OpenAI compatibility](https://docs.fireworks.ai/tools-sdks/openai-compatibility)
[^hyperbolic-openai]: [Hyperbolic serverless inference quickstart](https://docs.hyperbolic.xyz/docs/getting-started)
[^digitalocean-openai]: [DigitalOcean Serverless Inference endpoints](https://docs.digitalocean.com/products/inference/how-to/si-endpoints/)
[^cloudflare-openai]: [Cloudflare Workers AI OpenAI-compatible endpoints](https://developers.cloudflare.com/workers-ai/configuration/open-ai-compatibility/)
[^cloudflare-model-search]: [Cloudflare API — Workers AI model search](https://developers.cloudflare.com/api/resources/ai/subresources/models/methods/list/)
[^nvidia-hosted-chat]: [NVIDIA hosted NIM Chat Completions](https://docs.api.nvidia.com/nim/reference/z-ai-glm-5.2-infer)
[^modelscope-qwen]: [ModelScope Qwen3-8B API Inference example](https://www.modelscope.cn/models/Qwen/Qwen3-8B)
[^modelscope-limits]: [ModelScope API Inference limits and permitted use](https://modelscope.cn/docs/model-service/API-Inference/limits)
[^zai-chat]: [Zhipu Chat Completions API](https://docs.bigmodel.cn/api-reference/%E6%A8%A1%E5%9E%8B-api/%E5%AF%B9%E8%AF%9D%E8%A1%A5%E5%85%A8)
[^ollama-cloud]: [Ollama Cloud API access](https://docs.ollama.com/cloud)
[^ollama-openai]: [Ollama OpenAI compatibility](https://docs.ollama.com/api/openai-compatibility)
[^nebius-api]: [Nebius Token Factory API introduction](https://docs.tokenfactory.nebius.com/api-reference/introduction)
[^nebius-models]: [Nebius Token Factory — list models](https://docs.tokenfactory.nebius.com/api-reference/models/list-models)
[^nebius-flavours]: [Nebius Token Factory model flavours](https://docs.tokenfactory.nebius.com/ai-models-inference/overview)
[^nscale-chat]: [Nscale Chat API](https://docs.nscale.com/docs/use-cases/chat)
[^nscale-models]: [Nscale model overview and models endpoint](https://docs.nscale.com/docs/ai-services/models)
[^chutes-start]: [Chutes starter guide](https://chutes.ai/docs/guides/starter-guide)
[^chutes-tools]: [Chutes agents and tools](https://chutes.ai/docs/guides/agents-and-tools)
[^ovh-tools]: [OVHcloud AI Endpoints function calling](https://docs.ovhcloud.com/en/guides/public-cloud/ai-machine-learning/ai-endpoints-function-calling)
[^ovh-catalogue]: [OVHcloud AI Endpoints Catalog API](https://docs.ovhcloud.com/en/guides/public-cloud/ai-machine-learning/ai-endpoints-catalog-api)
[^hf-router]: [Hugging Face Inference Providers routing](https://huggingface.co/docs/inference-providers/en/index)
[^kilo-gateway]: [Kilo AI Gateway model routing](https://kilo.ai/docs/gateway/models-and-providers)
[^llm7-models]: [LLM7 model selectors](https://docs.llm7.io/guides/models)
[^opencode-zen]: [OpenCode Zen model gateway](https://opencode.ai/docs/zen)
[^requesty-router]: [Requesty inference API](https://docs.requesty.ai/api-reference/inference-apis)
[^requesty-openai]: [Requesty OpenAI integration](https://docs.requesty.ai/frameworks/openai)
[^vercel-routing]: [Vercel AI Gateway provider routing](https://vercel.com/docs/ai-gateway/models-and-providers/provider-options)
[^aion-api]: [Aion Labs API reference](https://api.aionlabs.ai/docs/api-reference/)
[^aion-terms]: [Aion Labs terms](https://api.aionlabs.ai/terms/)
[^agnes-public]: [Agnes AI public model surface](https://beta.agnes-ai.com/)
[^amd-token-factory]: [AMD Radeon Cloud Token Factory](https://developer.amd.com.cn/radeon/tokenfactory)
[^vllm-serve]: [vLLM OpenAI-compatible server](https://docs.vllm.ai/en/latest/serving/openai_compatible_server/)
[^mlx-serve]: [MLX-LM HTTP server](https://github.com/ml-explore/mlx-lm/blob/main/mlx_lm/SERVER.md)
[^llamafile]: [Mozilla llamafile](https://github.com/Mozilla-Ocho/llamafile)
[^lmstudio-server]: [LM Studio local server](https://lmstudio.ai/docs/developer/core/server)
[^llamacpp-server]: [llama.cpp HTTP server](https://github.com/ggml-org/llama.cpp/tree/master/tools/server)
[^jan-server]: [Jan local API server](https://jan.ai/docs/desktop/api-server)
