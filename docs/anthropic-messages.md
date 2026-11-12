# Anthropic Messages (`POST /v1/messages`)

`POST https://kaana.ai/v1/messages` speaks the Anthropic Messages dialect, so an
unmodified Anthropic SDK or Claude Code works with:

```bash
export ANTHROPIC_BASE_URL=https://kaana.ai
export ANTHROPIC_API_KEY=oxy_sk_...        # sent as x-api-key
# or ANTHROPIC_AUTH_TOKEN=oxy_sk_...       # sent as Authorization: Bearer
```

`model` is a Kaana model reference (`publisher/model[@revision]`) or a power
level, exactly as on `/v1/chat/completions`; a vendor model name is not mapped.
The contract is `packages/contracts/src/public/anthropicMessages.ts` (contract
set 8.3.0); the handler is `internal/publicapi/messages.go`.

## It is the `responses` operation

Nothing here is new to the signed contract. The handler asks Oxy for the same
`responses` grant `POST /v1/responses` does (`endpoint: "/v1/messages"`),
builds the same envelope (`apiFormat: responses`) and runs the same `admit`,
`execute`, settlement and timing line. Only the request and the answer are
translated. No Oxy grant operation was added and `internal/contract`'s grant and
envelope shapes are unchanged.

## Credentials

`bearerFrom` (`internal/publicapi/credential.go`) reads `Authorization: Bearer`
first and, when there is none, `X-Api-Key`. `anthropic-version` and
`anthropic-beta` are accepted and ignored. Both new headers are allowed by CORS
preflight.

## Supported

| Anthropic | Carried as |
| --- | --- |
| `system` (string or text blocks) | one `system` message |
| `messages` (string or blocks: `text`, `image`, `tool_use`, `tool_result`) | messages; a `tool_result` becomes a `tool` message and a `tool_use` an assistant tool call |
| `image`, `source.type: base64` (PNG, JPEG, WebP, GIF) | an inline image under the image rules of [ADR 0007](adr/0007-image-input.md) |
| `tools[].input_schema`, `tool_choice` `auto` / `any` / `none` / `tool` | tool definitions; `any` is `required` |
| `max_tokens` (required), `temperature`, `top_p`, `top_k`, `stop_sequences` | the same parameters |
| `output_config.effort` `low` / `medium` / `high` | `reasoning.effort` |
| `thinking: {type: "enabled", budget_tokens}` | `reasoning.maxTokens`; beside `output_config.effort` it is refused (they are alternatives); a route that cannot take a budget refuses it naming `reasoning.maxTokens` |
| `thinking: {type: "adaptive"}` | `reasoning: {}`: reasoning is returned, sized by the route |
| `thinking` and `redacted_thinking` blocks on an assistant message | a `reasoning` part; the block's `signature` (or `data`) is the opaque token, returned unmodified |
| `thinking: {type: "disabled"}`, `metadata.user_id` | accepted; `user_id` is neither forwarded nor stored |
| `stream: true` | SSE: `message_start`, `ping`, `content_block_start` / `_delta` / `_stop`, `message_delta`, `message_stop`; a failure after the first frame is one `event: error` frame and no `message_stop` |

The answer has `id` (`msg_` + the request id), `type`, `role`, `model` (the
revision that served it), `content` (`thinking`, `text` and `tool_use` blocks; a
`thinking` block appears only when the request asked for reasoning (`thinking`
enabled or adaptive, or `output_config.effort`), comes first, and carries
`signature` = the continuation token, so a stock Anthropic SDK continues a tool
turn by returning what it received; `redacted_thinking` is accepted back and
never produced),
`stop_reason` and `usage`. `stop_reason` maps the measured finish reason:
`stop` is `end_turn`, `length` is `max_tokens`, `tool_calls` is `tool_use`,
`refusal` and `content_filter` are `refusal`. `stop_sequence` is never produced:
the finish reason does not say which sequence ended the turn, so such a turn
reports `end_turn` and `stop_sequence: null`.

`usage` is derived from the measured units only: `input_tokens`,
`output_tokens` (reasoning tokens included, as Anthropic counts them) and
`cache_read_input_tokens` when a cache read was measured. A stream's
`message_start` carries zeros, because the totals are known only at the end; the
final `message_delta` carries them. The request id is in `Request-Id` and
`Kaana-Request-Id`.

Errors are `{"type":"error","error":{"type","message"},"request_id"}` with the
status the public code already has. A refusal of a parameter names it at the
start of the message (`messages[0].content[1].source: ...`).

| Public code | Anthropic `error.type` |
| --- | --- |
| `authentication_failed` | `authentication_error` |
| `permission_denied`, `insufficient_scope`, `policy_violation`, `commercial_permission_denied` | `permission_error` |
| `model_not_found` | `not_found_error` |
| `request_too_large` | `request_too_large` |
| `insufficient_balance`, `spending_limit_exceeded` | `billing_error` |
| `rate_limited`, `quota_exceeded` | `rate_limit_error` |
| `provider_timeout` | `timeout_error` |
| `provider_overloaded` | `overloaded_error` |
| `provider_error`, `service_unavailable`, `no_route_available`, `internal_error`, `cancelled` | `api_error` |
| every other code | `invalid_request_error` |

## A system message in `messages`

Claude Code sends a `role: "system"` message after the user's turn. It is
accepted with text blocks only (anything else is refused naming the block) and is
carried in the position it was sent; the Anthropic adapter hoists it into the
provider's system prompt like any system message.

## Accepted and ignored: `cache_control` and what Claude Code sends

Claude Code puts a `cache_control` marker on every request (`system[1]` in the
request that exposed this), so refusing it made the endpoint unusable by its
main client. It is accepted on the request, `system`, a tool, a content block or
a tool result's block, and **ignored**: Kaana forwards no cache hint, so no cache
write happens and `usage` stays exactly what the request cost. The caller does not
get a prompt-cache write discount; a read a provider caches on its own is still
reported as `cache_read_input_tokens`. A request whose marker was ignored is
answered with `Kaana-Ignored-Parameters: cache_control`, so it is never left to
inference. A key named `cache_control` inside a tool's own `input_schema` or
`input` is the caller's data and is not read. Three more members Claude Code sends are
accepted and ignored the same way: `context_management` (an Anthropic beta),
`thinking.display` and `tools[].defer_loading`. Each is a hint that changes how a
provider trims, shows or loads, never what a request means or costs; every
present one is named in `Kaana-Ignored-Parameters` (a comma-separated list in a
fixed order). Any other member the schema does not name is still refused.
Honouring the marker needs a priced cache-write unit (`cache_write_input_tokens`, contract 8.4.0) that Oxy rates and
the Anthropic adapter emits; nothing does yet (issue #269).

## Refused, not dropped

The schema is strict, so a parameter Kaana does not implement is refused with
`invalid_request` naming it.

- Every `thinking` member not named above (`display` is accepted and ignored: see above).
  (Reasoning was refused here until contract set 8.6.0; see
  [contract.md](contract.md#contract-set-860-reasoning-continuity-adr-0009-decision-2).)
- Server tools (`web_search`, `bash`, `text_editor`, ...), `disable_parallel_tool_use`,
  `document` blocks, an image `url` source (nothing fetches an image URL),
  `mcp_servers`, `container`, `service_tier`, `output_config.format` and every
  other member the schema does not name.
- A tool `description` over 32,768 characters (`toolDefinitionSchema`). The bound
  was 2,000 and was raised for this endpoint because Claude Code's built-in tool
  descriptions are far longer; the widening is backwards compatible (everything
  accepted before is still accepted).

`is_error` on a `tool_result` is accepted and not forwarded: the contract's tool
message has no error flag, so the result's content is what the model reads.

## `POST /v1/messages/count_tokens` is not served

Counting tokens honestly needs the serving model's tokenizer, which only a
provider call (or a vendored tokenizer per model line) provides. Kaana estimates
a hold from characters (`estimatedTokens`), which is not a count a client may
rely on, and calling a provider to count would be an unbilled, unauthorized
execution. The route is therefore absent and answers as any unknown `/v1/` path.

## Rules a reviewer applies

- **No grant operation, envelope field or signed shape specific to this
  endpoint.** It rides on `responses`; a change that needs one is a contract
  change that asks first.
- **A parameter the contract cannot carry is refused by name**, never dropped.
  `cache_control` is the one marker that is accepted and ignored instead (see
  above), because ignoring it cannot change what a request costs.
- **`usage` is measured units only.** Never an estimate, never a cache write
  folded into input.
- **No provider, deployment or key in any body or frame**, and no prompt in a
  log line, error or fixture. An image refusal names the part, not the image.
- **A new member of the request or an answer is Zod first**, then the fixture
  gate (`TestWritePublicWireFixtures`) proves the Go output parses under it.
