# ElevenLabs speech adapter

Kaana serves ElevenLabs text-to-speech through the `elevenlabs` provider, its
native `elevenlabs_speech` protocol and the `elevenlabs_models` discovery
profile. It executes one request family, `audio_speech`, through
`POST https://api.elevenlabs.io/v1/text-to-speech/{voice_id}`. It does not
stream, return timestamps, run Text to Dialogue, change voices, or transcribe.
Wire behaviour was reviewed against ElevenLabs' documentation on 2026-10-12
(sources at the end).

## The three gates

1. **Serving.** `internal/provider/elevenlabs` builds the call, meters it and
   classifies ElevenLabs' errors (below).
2. **Discovery.** `GET /v1/models` is authenticated and workspace-scoped.
   Without a key, ElevenLabs answers `404 workspace_not_found` for the
   anonymous workspace. With an invalid key it answers
   `401 authentication_error` (both measured 2026-10-12). The profile sends
   `xi-api-key` and reads a bare JSON array, which is not an OpenAI `data`
   list and is not paginated. It keeps a row only when
   `can_do_text_to_speech` is `true` and `requires_alpha_access` is not.
   Each kept row's `name` becomes the observed display name. Because
   ElevenLabs states the row is text-to-speech, the observed modalities are
   text in and audio out. `model_rates` multiplies the account's credits; it
   is not a published list price, so it is not read. A duplicate or blank
   `model_id`, a non-boolean flag, a non-array body or a list with no speech
   row fails that provider's discovery for the cycle.
3. **Publication.** `configs/model-attribution.json` attributes three ids.
   Each one names ElevenLabs as the publisher:

   | ElevenLabs id | Model line | Character limit |
   |---|---|---|
   | `eleven_multilingual_v2` | `elevenlabs/eleven-multilingual-v2` | 10,000 |
   | `eleven_flash_v2_5` | `elevenlabs/eleven-flash-v2.5` | 40,000 |
   | `eleven_v3` | `elevenlabs/eleven-v3` | 5,000 |

   Each id carries its own version. ElevenLabs documents no alias that
   re-points one. A superseded model keeps its id and is listed as deprecated
   with a replacement, which is how the Turbo ids are known to be absent.
   ElevenLabs does not say an id's weights never change, so these lines carry
   the same provider-managed-weights caveat as `x-ai/text-to-speech`
   (`inventory.md`, "xAI speech discovery candidate").

   These ids are absent on purpose:

   - `eleven_turbo_v2_5` and `eleven_turbo_v2` are deprecated and
     "functionally equivalent" to Flash.
   - `eleven_flash_v2` is the English-only subset of `eleven_flash_v2_5` at
     the same price. Oxy's catalogue carries no language field, so a
     price-ordered picker could not tell the two apart and would send Spanish
     to an English-only model.
   - `eleven_v4` and `eleven_v4_turbo` are documented through the Text to
     Dialogue API and its WebSocket, not this endpoint.
   - `eleven_v3_conversational` is a realtime WebSocket model.
   - Voice-changer, voice-design, sound, music and Scribe ids belong to other
     request families.

   `providerconfig.ClassifyModel` classifies every `elevenlabs` id as
   `audio_speech`, so the publisher can never attach one as a chat
   deployment. Each attributed id needs a reviewed row in
   `elevenlabs.MaxCharacters` (`TestCheckedInElevenLabsAttributionHoldsOnlyCurrentSpeechModels`).
   If the first production cycle's dropped-id warnings show `eleven_v4` with
   `can_do_text_to_speech: true`, adding it is a reviewed one-row change.

## Request mapping

- The signed upstream id is `model_id`. Nothing else is defaulted.
- **Voice.** The contract's `voice` is either a product voice or an exact
  ElevenLabs voice id, which is 20 ASCII letters and digits and is put in the
  path escaped:
  - `female` maps to Sarah (`EXAVITQu4vr4xnSDxMaL`).
  - `male` maps to George (`JBFqnCBsd6RMkjVDRZzb`).

  Both are premade voices that ElevenLabs' public `GET /v1/voices` labels
  female and male, and both list Spanish and English among their verified
  languages (read 2026-10-12). Any other id is validated by ElevenLabs
  against the workspace's library. An unknown id comes back as
  `voice_not_found`, which is a non-retryable `invalid_request`. Any other
  value is refused before sending.
- **Format.** `responseFormat: mp3` is sent as `output_format=mp3_44100_128`,
  ElevenLabs' documented default, available on every tier. Every other
  contract format is refused for now.
- **Speed.** `speed` is sent as `voice_settings.speed`, and only when the
  caller set it. ElevenLabs bounds it to 0.7–1.2, and a value outside that
  range is refused. ElevenLabs applies the voice's stored settings to every
  other field.
- **Length.** The text must be 1 character to the model's documented limit
  (table above). The adapter counts UTF-16 code units, the unit Oxy's public
  ceiling uses. That count is never below ElevenLabs' own, so a request that
  passes here is never refused upstream for length.
- **Refused.** Streaming, `audioOutput`, reasoning, tools, sampling,
  `maxOutputTokens`, `responseFormat` (structured output) and every
  non-speech family are refused before anything is sent.
- **Not sent.** `enable_logging=false` (ElevenLabs' zero-retention mode) is
  never sent, because ElevenLabs reserves it for enterprise customers. The
  Oxy workspace is not one, so ElevenLabs keeps its history of these
  requests. Oxy's provider record must say so.

## Output and usage

The body must be `audio/mpeg` with MP3 framing (an ID3 tag or a frame sync)
and at most 20 MiB. It is emitted as independently base64-encoded `audio`
events of at most 49,152 bytes.

ElevenLabs documents no usage object or header for this endpoint. It bills
the characters of the text it accepted. Kaana therefore reports
`characters`, its own UTF-16 count of the text it sent, as `oxy_measured`.
The units are recorded as soon as ElevenLabs answers 200 and before the body
is read, so a cut connection, invalid audio or a disconnected customer still
settles the characters ElevenLabs billed. A refused request is never metered.

ElevenLabs bills in credits. Its credit-per-character rate depends on the
model: Flash is documented as half the price per character of Multilingual
v2. The pay-as-you-go list prices are per 1,000 characters: Flash and Turbo
$0.04, Multilingual v2 and v3 $0.08 (https://elevenlabs.io/pricing/api,
read 2026-10-12). A Kaana rate card is keyed by deployment ids the publisher
has not yet named, so until one is reviewed the cost of an ElevenLabs attempt
is `unknown`, never zero (`cost.md`).

## Errors

The adapter classifies by ElevenLabs' `detail.type` and `detail.code`, never
by message prose. It falls back to the legacy `detail.status` that
ElevenLabs still sends. What each verdict means for the key is
`provider.Walk`'s.

| ElevenLabs answer | Kaana code | Key verdict |
|---|---|---|
| 402 `payment_required` / `insufficient_credits`; any `quota_exceeded` | `provider_billing_refused` | exhausted |
| 401 `authentication_error` (`invalid_api_key`, `needs_authorization`, expired) | `provider_credential_invalid` | rejected |
| 403 `authorization_error`: `forbidden`, `insufficient_permissions`, `workspace_access_denied`, or no code | `provider_credential_invalid` | rejected |
| 403 `voice_access_denied`, `model_access_denied`, `feature_not_available`, `subscription_required` | `permission_denied` | request fault |
| 429 `rate_limit_exceeded`, `concurrent_limit_exceeded` | `rate_limited` (honours `Retry-After`) | healthy |
| 429 `system_busy`; 503 | `provider_overloaded` | healthy |
| 404 `model_not_found` | `model_not_found` | request fault |
| 404 `voice_not_found`; 400 `validation_error`/`invalid_request`; 422 | `invalid_request` | request fault |
| other 5xx | `provider_error` | healthy |

The production key is restricted to Kaana's NAT egress addresses, and
ElevenLabs answers 403 from any other address
(https://elevenlabs.io/docs/overview/administration/workspaces/api-keys).
That 403 is credential configuration, never the customer's fault, so it
retires the key and trips the deployment's breaker rather than blaming the
request. If the egress IPs change, every ElevenLabs request fails this way
until the allowlist is updated in ElevenLabs. Messages reach the customer
after the key is removed by exact match.

## Health

Health never synthesizes speech, because a probe would spend credits. It
stays degraded until a signed canary through Oxy proves the key can speak,
as for Deepgram and OpenAI audio.

## Operator configuration and enablement

- **Credential.** `elevenlabs` / `e1ed4df9-3274-4833-8eff-a946d761d36c` is
  custodied as KMS ciphertext in Kaana's PostgreSQL store. It is enabled with
  metadata `capacityCategory: promotional`, ElevenLabs' Startup Grant: 33,000,000
  credits for 12 months with Scale-tier features. Its expiry evidence is
  recorded as about 2027-10-12. Business-only features are not included. The
  key is IP-restricted to the NAT egress addresses, so both the serving
  process and the publisher must leave through that NAT. The legacy
  imported row `6e4abb22-af03-46fb-95d9-b2e4286657f2` is disabled.
  ElevenLabs is therefore a single-key provider: every published deployment
  resolves to that key by the provider default (`key-pools.md`) and needs no
  binding. Adding a second enabled `elevenlabs` key removes the default, so
  bind every published deployment first.
- **Discovery.** The key is the publisher's reviewed discovery identity
  (`.github/credential-admin-operations.json`, `discoveryCredentialIds`).
- **Capacity evidence.** `GET /v1/user/subscription` (`character_count`,
  `character_limit`, `next_character_count_reset_unix`) is ElevenLabs' own
  statement of remaining capacity. It is unmetered. It is evidence for
  credential-capacity reconciliation, not something serving reads.
- **Infrastructure.** oxy-infra (`terraform-uswest2/app-kaana.tf`) owns
  `KAANA_PROVIDERS` and `KAANA_DISCOVERY_PROVIDERS`. Enabling ElevenLabs adds
  `elevenlabs` to both, with `KAANA_PROVIDER_ELEVENLABS_PROTOCOL=elevenlabs_speech`,
  `KAANA_PROVIDER_ELEVENLABS_BASE_URL=https://api.elevenlabs.io/v1` and the
  discovery key id above. Apply it only after an image containing this
  adapter is deployed: an older image refuses the unknown protocol at
  startup.
- **Catalogue.** Oxy admits speech lines only through its reviewed catalogue
  (`x-ai/text-to-speech` is the precedent), never through the automatic
  sync. That needs the exact deployment ids from the first production
  snapshot, ElevenLabs' policy facts and a reviewed `characters` price.

## Validation and sources

The real-wire fake tests cover:

- the path, query, headers and body ElevenLabs documents;
- both product voices, an exact voice id and speed;
- every refusal, with nothing sent;
- each error class with its credential verdict, including the measured 401
  bodies;
- credential redaction;
- units surviving a sink failure and invalid audio;
- cancellation reaching the upstream;
- endpoint identity and health.

The publisher tests cover the discovery filter, its refusals and the
snapshot the checked-in attribution builds. They use synthetic credentials.
No call has been made with the production key, because it answers only from
Kaana's egress. The first signed production canary is that evidence.

- [Create speech](https://elevenlabs.io/docs/api-reference/text-to-speech/convert)
- [List models](https://elevenlabs.io/docs/api-reference/models/list)
- [Models, character limits and deprecations](https://elevenlabs.io/docs/overview/models)
- [Authentication](https://elevenlabs.io/docs/api-reference/authentication), [API keys and IP allowlisting](https://elevenlabs.io/docs/overview/administration/workspaces/api-keys)
- [Errors](https://elevenlabs.io/docs/developers/resources/errors)
- [Speed](https://elevenlabs.io/docs/overview/capabilities/text-to-speech/best-practices)
- [User subscription](https://elevenlabs.io/docs/api-reference/user/subscription/get)
- [API pricing](https://elevenlabs.io/pricing/api)
