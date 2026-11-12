# Provider onboarding log

> **Historical record.** The dated reviews moved out of [provider-onboarding.md](provider-onboarding.md),
> which keeps the rules, the per-provider facts and the gates. Each section is as it stood on its
> date; check the live inventory and the provider's own documentation before relying on one.

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
| LLM7 | LLM7 documents the moving selectors `default`, `fast` and `pro` alongside a live catalogue of concrete model ids. Its own service description says requests are shuffled across providers, while a catalogue row does not identify the exact backend provider that serves a request.[^llm7-models][^llm7-gateway] | Selectors can change the model, and concrete ids still do not bind one downstream provider/cost identity. Neither shape is an immutable direct-provider deployment Kaana can meter and reconcile. |
| OpenCode Zen | OpenCode describes Zen as an AI gateway and publishes a mixed endpoint table, including Responses-only entries.[^opencode-zen] | Gateway identity is not downstream provider identity, and the mixed wire is not the one shared Chat Completions adapter contract. |
| Aion Labs | The API reference fixes a Chat Completions wire and publishes prices, but its model list is public rather than an authenticated entitlement list, and Aion's terms define the service as routing requests to upstream AI model providers and third-party hosting infrastructure.[^aion-api][^aion-terms] | The response contract does not expose a stable downstream provider/cost identity that Kaana can bind and reconcile. An Aion-branded model name is not proof of one direct deployment. |
| Agnes AI | The provider-owned public surface mixes Agnes, OpenAI, Google and other publishers, while no stable provider-owned Chat Completions and model-list contract was found.[^agnes-public] | A marketing catalogue cannot substitute for authenticated wire, lifecycle and billing semantics. |
| GLHF | No current provider-owned API contract was found that fixes auth, streaming usage, model identity and price. | The endpoint lead alone is insufficient evidence. |
| AMD Radeon Cloud | The provider-owned Token Factory page is interactive and did not publish a reviewable API schema, authentication contract, model lifecycle or price/quota semantics.[^amd-token-factory] | An endpoint inferred from a portal example is not a deterministic production contract. No AMD slug or model attribution is added. |
| Ollama Cloud | Ollama documents the remote service at `https://ollama.com/api`, with bearer authentication and the native `/api/chat` and `/api/tags` wire. Its OpenAI-compatibility documentation demonstrates the local `http://localhost:11434/v1` server instead. The native catalogue exposes a content digest, but the chat request documents only a model-name selector and Ollama explicitly retires cloud models over time.[^ollama-cloud][^ollama-openai][^ollama-tags][^ollama-openapi] | Pointing the OpenAI adapter at an undocumented remote `/v1` path would be guesswork. A native adapter alone would still not prove that a previously published model name resolves to the same digest; Kaana needs a provider-supported immutable chat selector before it can publish a pinned reference. |
| vLLM, MLX, llamafile, local Ollama, LM Studio, llama.cpp and Jan | Their official projects describe operator-run local or self-hosted servers.[^vllm-serve][^mlx-serve][^llamafile][^ollama-openai][^lmstudio-server][^llamacpp-server][^jan-server] | They have no provider-owned global HTTPS identity, account catalogue or price. Kaana's arbitrary explicit HTTPS provider configuration remains the correct escape hatch for a separately operated deployment; none becomes a global built-in. |

Requesty, Vercel, Hugging Face, Kilo, LLM7, OpenCode, Aion Labs and the other
excluded candidates also receive no `configs/model-attribution.json` block.
That absence is intentional: a public model list, promotional free tier or
display name cannot establish immutable weights or the cost paid to the direct
provider. `internal/providerconfig` and the checked-in attribution tests pin
this fail-closed disposition.

## 2026-09-30 catalogue review

Every discovering provider was re-read against its own documentation (for
OpenRouter, its public `GET /api/v1/models`). The same three gates applied;
nothing was copied from a third-party list.

**OpenRouter.** 39 newly served ids have text output and a dated canonical
slug, or are a `:free` route of such a line: Anthropic Claude Fable 5.1,
Opus 5.5 and Sonnet 5.5; OpenAI GPT-6 Astra, GPT-6 Sol, GPT-6 Luna and
GPT-6.1 Sol with their `-pro` routes (as the GPT-5.6 `-pro` rows already
were); Google Gemini 3.8 Flash; Cohere Command A+; DeepSeek V4.1 Flash; xAI
Grok 4.7; Qwen3.8 Max 0902, Max Prime and Omni Flash plus the free Qwen3.8
27B route; and new rows from Fireworks, Inception, inclusionAI, Inference.net,
Meta, Nex AGI, NVIDIA, Perceptron, PrismML, Upstage, Xiaomi and Z.ai.[^openrouter-models]
OpenRouter's own descriptions exclude the rest: Aion 3.5 is "a multi-model
... system", Sakana Fugu is "a learned multi-agent orchestration system",
Unbiased Pareto is "a multimodal composite model", and TypeSafe's Jev Router,
`openai/gpt-chat-latest`, Tencent Hy4 preview, `stealth/space-bunny-alpha`,
`~...latest`, `openrouter/*` and every `:batch` row fail the existing rules.

**Withdrawn upstream, retired here only where the snapshot allows.** Thirty
attributed OpenRouter ids no longer appear in the live catalogue, Groq shut
`qwen/qwen3.6-27b` down on 2026-09-14 and no longer lists `allam-2-7b`, and
Cerebras withdrew `gemma-4-31b` from Shared Inference on 2026-09-03.[^groq-deprecations][^cerebras-deprecations]
Three of the OpenRouter rows (`inclusionai/ling-3.0-flash-fin:free`,
`minimax/minimax-m2.7:free`, `minimax/minimax-m3:free`) are in no checked-in
deployment and are removed. The others are still named by
`configs/inventory.json`, a measured snapshot this review does not hand-edit:
their attribution stays so the snapshot remains readable, and the publisher
already withdraws each one on the first cycle whose discovery no longer
returns it. They can leave this table in the change that refreshes the
snapshot.

**Direct providers.**

- Cerebras: `qwen-3.8-27b` is Production and attributed to the existing
  `qwen/qwen3.8-27b` line. Cerebras commits to serving the original weights
  for an existing model id.[^cerebras-models]
- xAI: `grok-4.7` is attributed like the other `grok-4.x` rows; its model page
  lists no alias.[^xai-grok47]
- Groq: the only new text ids, `minimaxai/minimax-m2.7` and `qwen/qwen3.8-27b`,
  are Preview and stay absent.[^groq-models]
- OpenAI, Mistral, SiliconFlow, Nebius and Alibaba: see their sections.
- DeepSeek, SambaNova: no new fixed production id (see their sections).
  SambaNova's only additions, `MiniMax-M3` and `gemma-4-31B-it`, are Preview.[^sambanova-catalogue]
- Nscale: its public pages name no exact id beyond the attributed example,
  which is still current; the full list is visible only to an authenticated
  account.

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
- Groq's Compound ids are systems that choose among models and tools, and
  Orpheus uses a speech endpoint the Chat Completions adapter cannot
  represent.[^groq-systems][^groq-speech] Whisper (`whisper-large-v3`,
  `whisper-large-v3-turbo`, attributed to OpenAI) is served since 2026-10-16
  as file transcription on Groq's OpenAI-compatible `/audio/transcriptions`
  (`internal/provider/openaitranscription`), classified by
  `providerconfig.ClassifyModel` and taken as `providerconfig.TranscriptionCapabilities`
  states: Groq's containers, Whisper's ISO 639-1 languages (a client's BCP 47
  tag is sent as its code, or none for the model to detect), a prompt, and its
  ten-second minimum billed length.[^groq-transcription]
- Groq's Llama Prompt Guard 2 (`meta-llama/llama-prompt-guard-2-22m`,
  `-86m`) is an mDeBERTa classifier with a 512-token window. Groq serves it at
  `/chat/completions`, but it holds no conversation and fails a streamed chat
  request, and the contract has no classification family, so the family gate
  (`providerconfig.ClassifyModel`) refuses it under every adapter and the
  publisher drops it as inexecutable and names it (2026-09-30). Its attribution
  rows stay only while the measured `configs/inventory.json` still names them.
  `openai/gpt-oss-safeguard-20b` is NOT excluded: Groq documents it as a
  generative reasoning model (tools, JSON schema, reasoning effort) that
  classifies by writing text, which is ordinary chat.[^groq-moderation]
- Groq lists `qwen/qwen3.8-27b` as Preview, so the direct Groq route remains
  unattributed even though the fixed model line is available elsewhere.[^groq-qwen]
- xAI's Imagine ids belong to image- or asynchronous video-generation APIs,
  not the text stream Kaana currently normalizes.[^xai-image-models][^xai-video]

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
The official examples provided two fixed Meta ids for Nebius and one for
Nscale, so those exact ids were attributed to canonical model lines already
established by existing routes. Nebius removed `meta-llama/Llama-3.3-70B-Instruct`
from Serverless on 2026-08-31 and its public catalogue no longer lists
`meta-llama/Meta-Llama-3.1-70B-Instruct`, so on 2026-09-30 both were replaced by
the exact ids Nebius's own 2026 deprecation notices name as replacements:
`deepseek-ai/DeepSeek-V4-Flash-0731`, `MiniMaxAI/MiniMax-M3`,
`nvidia/nemotron-3-super-120b-a12b`, `nvidia/Nemotron-3_5-Lightning`,
`openai/gpt-oss-120b` and `Qwen/Qwen3.5-397B-A17B`. `zai-org/GLM-5.3` is left
out because Nebius's catalogue links it to a third-party NVFP4 quantisation
rather than Z.ai's weights.[^nebius-deprecation-aug][^nebius-deprecation-jun][^nebius-catalogue] They still create no deployment unless the authenticated
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
`default`, `fast`, `pro`, `kilo-auto/*` and mixed-protocol model entries do not
meet Kaana's single-provider deployment identity. LLM7 also publishes concrete
model ids, but its provider-owned surface says requests are shuffled across
providers and its model rows do not bind one exact backend provider to the
request. An operator can still declare a reviewed HTTPS origin explicitly, but
publication needs a router-aware contract first. GLHF is also excluded because
no current provider-owned wire contract could be verified.[^hf-router][^kilo-gateway][^llm7-models][^llm7-gateway][^opencode-zen]

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
base that Kaana can pin without inference. Its native model list returns a
content digest beside each model name, but the documented chat request still
accepts a model name rather than a digest-addressed immutable selector.[^ollama-cloud][^ollama-openai][^ollama-tags][^ollama-openapi]
