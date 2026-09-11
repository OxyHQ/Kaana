# Provider adapters

The `provider.Adapter` interface, the two protocol adapters, and the conformance suite every adapter must pass.

## The provider adapter interface

```go
type Adapter interface {
	Provider() contract.ProviderSlug
	Translate(request *contract.Request, route Route) (*Call, error)
	Stream(ctx context.Context, call *Call, out Emitter) (Outcome, error)
	Health(ctx context.Context) Health
}
```

- **`Provider`** names the slug every event and usage record attributes work to.
  It comes from the adapter, not its registration site, so a mis-registration
  cannot mislabel a receipt.
- **`Translate`** is pure. A request the provider cannot express is refused
  before anything is spent upstream, and a pure translation is testable with no
  network — which is what makes covering that refusal cheap.
- **`Stream`** is execution, streaming, cancellation and usage measurement
  together, because they share one lifetime: the units are only correct if they
  come from the same read loop that saw the last frame before the cut.
- **`Health`** must be answerable without a customer request, so it cannot be
  folded into `Stream`.

What an adapter deliberately does **not** do: allocate ids, assign sequence
numbers, decide terminality, emit `done`/`error`/`route_switch`, resolve a model
reference to an upstream model id, apply routing policy, or decide what a
failure means for the CREDENTIAL that produced it. That last one is
`provider.Walk` and `provider.CredentialVerdictFor`: an adapter says which
provider error this was, in the contract's vocabulary, and the pool decides what
follows — because "this key is spent" and "this key is refused" are opposite
decisions about a pool, and an adapter free to restate them is free to restate
them differently. Those are one
implementation in the executor, not one per provider. Adapters report semantic
content through `Emitter`, which stamps `requestId`, `sequence` and
`schemaVersion` itself — removing the whole class of bug where one provider's
events are unattributable or repeat a sequence.

## The protocol adapters

Two protocols are implemented. Their shared conformance suite keeps the adapter
boundary independent of either provider's wire format.

### `openaicompat` — OpenAI Chat Completions

Kaana's OpenAI Chat Completions implementation is protocol-shaped rather than
provider-shaped. Every compatible provider is a `Config` and a conformance
registration, while provider-specific request policy remains explicit. Every
OpenRouter inference request carries exactly
`provider: {zdr: true, data_collection: "deny", require_parameters: true}` as
documented by [OpenRouter's provider-routing API][openrouter-routing]. Kaana
constructs that typed object internally; it is neither an Oxy contract field nor
an operator/caller passthrough, so an unknown inbound `provider` object cannot
weaken it. The normalized OpenRouter API origin is reserved for that exact slug,
and that slug is bound back to the reserved origin; a custom compatibility slug
cannot bypass the policy by borrowing the URL, and another provider cannot
receive the field by borrowing the slug. Other OpenAI-compatible providers
receive no `provider` field.
`TestOneProtocolServesSeveralProviders` runs the full suite under seven more
slugs to keep the shared protocol claim honest, while a real-wire fake pins the
provider-specific request body.

Alibaba Model Studio and Cloudflare Workers AI use the same adapter without
gaining a guessed global origin. Their official URLs contain a workspace/region
or account id, so Kaana compiles the reviewed protocol and exact endpoint shape
while requiring the non-secret base URL at runtime. Both slugs run the complete
synthetic conformance suite; provider enablement still requires a scrubbed real
wire capture, and catalogue support remains a separate gate.

**Protocol invariants.** The raw upstream stream never crosses Kaana's boundary:
the adapter normalizes events and usage, propagates cancellation and classifies
provider failures. Streamed requests ask for `stream_options.include_usage`,
and an absent sampling parameter stays absent so the selected deployment's own
default applies. The adapter never invents `temperature` or `max_tokens`.

**Missing-usage fallback.** Asking for `stream_options.include_usage` is not a
guarantee: gateways can strip the terminal usage frame, and non-streamed
responses can omit the object too. A successful answer is therefore never
turned into `internal_error` only because the provider supplied no counters.
The executor emits and settles a deterministic `usageSource: "estimated"`
fallback derived from the normalized request and the output events that were
actually delivered. It always counts the upstream request; it estimates ASCII
text at four characters per token and non-ASCII text at one code point per
token, keeps visible and reasoning output disjoint, and counts tool JSON without
retaining prompt or completion text. Provider-reported units always win when
present. This creates units, not prices: Oxy remains the only
customer-pricing authority and existing Kaana rate cards remain the only source
of operator-side upstream rates.

The same rule covers a stream that delivered output and then failed or was
cancelled: successfully accepted delta/reasoning/tool-call events prove partial
work and produce estimated units for the report and provider-cost record. A
failure before the first accepted output stays unitless instead of charging from
the input merely because it was present. Once a sink has returned an error,
Kaana still builds that report but does not attempt a fallback usage write to a
destination already known to be broken.

The fallback is intentionally explicit about its limits. It cannot reconstruct
a model's exact tokenizer, cached-token attribution, image tokenization, or
audio/video duration from transport bytes. Inline base64 is excluded from text
estimation (counting it would price encoding size as model input), and the
fallback does not map an input image onto the ambiguous output-oriented
`images` unit. Multimedia dimensions therefore need provider usage. Settlement
can distinguish and later reconcile an estimate instead of mistaking it for the
provider's invoice.

### `anthropic` — the Messages API

Kaana's Anthropic Messages implementation is the independent protocol case that
keeps `provider.Adapter` from describing only OpenAI Chat Completions.

**Protocol differences.** It disagrees with chat completions on every axis the interface
names — named SSE events instead of one repeated frame closed by `[DONE]`;
indexed content **blocks** whose kind is declared once in the event that opens
them; reasoning as a block type rather than a field; usage split across two
events with a **cumulative** output count; a failure that can arrive *inside* the
stream after a 200; `x-api-key` with a mandatory `anthropic-version` instead of a
bearer token; the system prompt hoisted out of the message list; a tool result
carried as a user message; and `max_tokens` **required**. A second
OpenAI-compatible provider would have exercised none of that.

Its usage fields also nest the *other way round*, which is the finding with money
attached: `input_tokens` **excludes** cached tokens where an OpenAI-compatible
`prompt_tokens` includes them, while `output_tokens` includes reasoning exactly
as `completion_tokens` does. So one of the two normalising subtractions the
contract's partition needs applies here and **the other must not** — and an
adapter written by copying the first one would under-report every cached request.

**Protocol invariants.** The adapter preserves text, tool calls, reasoning, stop
reasons and usage through Kaana's normalized stream. It handles streamed and
non-streamed responses, never invents `temperature`, and treats Anthropic's
required `max_tokens` as an explicit translation requirement.

**What it refuses rather than inventing.** `max_tokens` is required upstream and
optional in the contract, so a request that omits `maxOutputTokens` is refused
with the field named. Choosing a ceiling here — or per deployment, which only
moves the invention into a config file — would truncate an answer the customer
asked to be unbounded and report success. That is item 14 below.

There are no provider credentials here, in the tests, or in CI. Both adapters
are exercised against a fake upstream that speaks the real wire format,
including its habit of echoing the request's credential header back inside an
error message.

### Cross-protocol invariants

Both protocol implementations use the same `Provider`/`Translate`/`Stream`/
`Health`, `Call`, `Route` and `Outcome` boundary. Three surrounding invariants
are required for that boundary to remain protocol-independent:

- **`Emitter` has nowhere to put provider-opaque block metadata.** A thinking
  block's `signature` is what makes multi-turn tool use with reasoning work, and
  no contract stream event has a field for it. The adapter reads it so it cannot
  be mistaken for output, and drops it. Item 17.
- **The conformance suite accepts every refusal the protocol needs to express**,
  as a list rather than a single protocol-shaped special case.
- **Credential redaction could not be left to the contract's pattern.** It is
  keyed to bearer-token shapes; against `x-api-key: <value>` it matches the
  marker and not the value, so redacting *removes the evidence and keeps the
  credential*. `provider.RedactSecret` removes the adapter's own key by exact
  match first. Item 18.

## The conformance harness

`internal/provider/conformance` is the suite an adapter must pass. An author
supplies five things — how to build the adapter, how to start a fake upstream
speaking that provider's **real** wire format, the route it serves, the requests
the provider genuinely cannot express, and what its fake upstream physically
consumed and produced — and gets back:

slug validity and stability · event framing (one `start` first, monotonic
sequences, `requestId` and `schemaVersion` on every event, exactly one terminal)
· a revision-pinned resolved model · the same normalized shape from a
non-streamed upstream · **units that partition the request**, on both read paths
· a provider that reports no usage settling as an estimate · tool calls a client
can reassemble · a transient throttle classified retryable · an exhausted
account classified non-retryable · **a refused PLATFORM credential classified
non-retryable and still attributable** · **a failure that arrives after the
response started** · **the configured credential never reaching the customer**, with a
control asserting the upstream actually echoed it AND that the customer still
receives the upstream's diagnostic rather than losing it to the contract's
refusal · one refusal per class, each
spending nothing upstream and naming the field at fault · the generic pool
walker's bounded exhaustion semantics (production supplies an exact one-key
view) · **a refused credential NOT walking the
pool** · **a request the provider itself refused retried nowhere** · **each
credential spent at most once** on a pool where every key is exhausted ·
cancellation, with its control · health with and without a credential.

The author now supplies at least TWO credentials rather than one. A pool of one
cannot tell an adapter that rotates on exhaustion from one that cannot rotate at
all, and two keys that are the same string cannot tell "the second key served
it" from "the first key was retried" — both are refused by the suite before any
of its checks run.

The suite drives the adapter through the **real executor**, because an adapter is
only correct in the shape it is actually used.

**What the second adapter changed here, and which half of it was general.** Six
changes, and the distinction matters:

| Addition | General, or the suite having been OpenAI-shaped? |
|---|---|
| Units partition the request (`StreamedUsage`) | **General.** It is the contract's own rule, and it caught the double-charge on the *first* adapter when that adapter's subtraction was removed — the suite had no check that would have. |
| A failure arriving mid-stream, after a 200 | **General.** Both protocols can do it; neither adapter handled it before, and `openaicompat` was reporting a truncated answer as a completed one. |
| A refused platform credential (`provider_credential_invalid`) | **General**, and newly expressible: the code landed in `@oxy.so/contracts@0.28.0` while this branch was open. Both adapters were reporting it as retryable. |
| A list of refusals rather than one | **The suite was OpenAI-shaped.** One slot fit because the first adapter had exactly one refusal class. |
| `maxOutputTokens` populated in the fixture | **The suite was OpenAI-shaped**, in the sense that a minimal fixture only passes for a provider that requires nothing the contract makes optional. Populating optional fields is this repository's own rule anyway. |
| "an exhausted quota on the *same status*" | **Was OpenAI-specific prose.** The invariant is that an adapter tells a throttle from an exhausted account; that they share a status is one provider's habit. Wording only. |

## Rules a reviewer applies

- **One implementation of `provider.Adapter` and one fake upstream per
  provider.** If a change to add a provider touches the executor, the stream
  framing or the receipt shape, the abstraction is wrong — fix that instead.
- **Every adapter passes `internal/provider/conformance` before it is
  registered.** The fake upstream must speak the provider's **real** wire format;
  a fake speaking the normalized contract tests nothing, because translation is
  the half with no schema to check it.
- **Adapters never** allocate ids, assign sequence numbers, decide terminality,
  emit `done`/`error`/`route_switch`, resolve a model reference to an upstream
  model id, or apply routing policy.
- **Refuse in `Translate` what the provider cannot express**, with a
  non-retryable code and the field named. Silently dropping a parameter changes
  what the model does while reporting success.
- **An adapter classifies its own failures, and stops there.** What that
  classification means for the KEY is `provider.Walk`'s, once, for every adapter
  — an adapter that reimplemented the rotation rules would be free to
  reimplement them differently. Adapters supply `Send`, `Refuse` and
  `TransportFailure`; `Refuse` closes the response body it read.
- **Classify from the provider's own error TYPE first.** Never infer
  retryability from an HTTP status: a 429 from an exhausted daily quota and a
  429 from a burst limit are the same status and opposite answers — and a
  failure arriving mid-stream, after a 200, has no status at all. Every
  streaming protocol can fail that way, and an adapter that reads the frame it
  cannot use and stops reports a truncated answer as a completed one.
- **The contract's usage units PARTITION a request, and the normalising
  arithmetic is NOT portable between adapters.** An OpenAI-compatible
  `prompt_tokens` includes its cached tokens; Anthropic's `input_tokens` excludes
  them and its `output_tokens` includes reasoning. Copying one adapter's
  subtraction into another mis-bills silently, because a nested report and a
  disjoint one are the same non-negative integers. State the PHYSICAL request in
  the conformance subject and let the suite do the arithmetic.
- **An adapter redacts its OWN credential by exact match; the contract's pattern
  is a last-resort REFUSAL and never the control.** `provider.RedactSecret`
  removes the value the adapter is holding — the only thing that works on a
  credential with no marker and no issued-token prefix, which the published
  pattern states it cannot see. Relying on the refusal instead loses the whole
  diagnostic, which the conformance suite fails you for.
- **Never redact by replacing the span a credential pattern matched.** The span
  is the MARKER and the secret is what follows it, so a span redaction converts
  "this string is dangerous" into "this string is fine" with the key still in
  it. `contract.SafeErrorText` withholds the whole message or none of it
  (`architecture.md`, finding 18).
- **A parameter the provider REQUIRES and the contract makes optional is
  refused, never supplied.** Choosing it at the adapter, or per deployment,
  changes what the model does while reporting success.
- **`Stream` returns the units it measured even when it fails.** A partial
  stream is a settlement case; an adapter that returns nothing on cancellation
  makes an exact refund impossible.
- **`ctx` reaches the upstream HTTP request.** That is the entire cancellation
  design, and it is why an adapter cannot decline to honour it.
- **Never invent a default the caller did not send.** An absent sampling
  parameter means the route's own default.
- **Redact upstream error text before emitting it.** Provider errors routinely
  echo the request that caused them; the contract *rejects* an error body whose
  text looks like a credential, so an unredacted one loses the customer their
  diagnostic entirely. The conformance suite covers this with a control proving
  the upstream really echoed one.

[epic]: https://github.com/OxyHQ/oxy/issues/972
[adr0005]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0005-oxy-is-the-single-control-plane.md
[adr0006]: https://github.com/OxyHQ/OxyHQServices/blob/main/docs/adr/0006-oxy-kaana-boundary.md
[openrouter-routing]: https://openrouter.ai/docs/guides/routing/provider-selection
