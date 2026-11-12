# Opt-in JSON repair

Models sometimes return JSON that is almost valid even when `json_object` or
`json_schema` was requested: wrapped in a markdown fence, with prose around it,
with a trailing comma, or missing the closers a normal stop forgot. Kaana can
repair that without a retry, when the caller asks. Decision:
[ADR 0009](adr/0009-contract-gaps-found-comparing-with-openrouter.md), decision 5;
wire: [contract.md](contract.md) (contract set 8.9.0).

## The control

`POST /v1/responses`, `responseFormat: {type: 'json_object' | 'json_schema', ..., repair: true}`.
Default off: a request without it, or with `false`, is byte-for-byte what it was.

- Refused as `invalid_request` naming `responseFormat.repair` on a `text` format,
  on a non-boolean, and with `stream: true` (repair needs the whole answer).
- Not on chat completions (a standard client has no typed field for it) and not
  on `/v1/messages` (that dialect has no JSON format).
- It is Kaana-side: not a request-parameter word, it does not narrow route
  selection, and it is not in the signed request, the grant or anything Oxy or a
  provider sees.

## What runs, and when

In the public API fold (`collected.message`), on the final assembled text of a
non-streaming answer, and only when the finish reason is `stop` and the answer
carries no refusal, no tool call and no audio transcript. An answer that ended
`length` (the token limit), `content_filter` or `refusal` is never touched: a
cut-off answer is not a JSON answer that forgot its closers, and a repair would
make it look complete. Reasoning blocks and tool calls are untouched; only the
text part changes.

`internal/jsonrepair.Repair`, a pure function, in this order, stopping at the
first that yields one valid JSON value:

1. trim whitespace and a byte order mark;
2. strip one surrounding markdown code fence;
3. extract the first balanced object or array from surrounding prose (string
   and escape aware). What surrounds it must be prose: a second structure or a
   stray closer makes this step decline rather than drop data;
4. remove trailing commas before `}` or `]`;
5. close an unterminated string and unclosed brackets at the end of the text.

Text that already parses is returned byte-identical, and text no step can make
valid is returned untouched. Nothing is reordered, requoted or invented: a key
with no value, a half-written literal or a mismatched bracket is not completed.
Text over 4 MiB is not attempted. Known ambiguity: a prose footnote such as
`[1]` before an object is skipped, but prose around a lone array can only be
told from the answer by position, so the first balanced array wins.

There is no schema validation. Kaana carries no JSON Schema validator, so a
repaired answer is valid JSON, not necessarily valid against `schema`.

## The marker

`Kaana-Repaired: true` on the response when the text changed (exposed to
browsers by CORS), absent otherwise. A header and never a body member, so every
reader of an older contract parses the answer. Usage and the cost row are the
provider's and are unchanged. The text is never logged; one line with the request
id counts the repair.

## Rules a reviewer applies

- Repair is never default-on and never reaches an adapter, a provider or Oxy.
- Never repair when the finish reason is not `stop`.
- Never alter text that already parses; never invent a value.
- Never log or persist the text before or after repair.
- A new step must keep the fuzz property: the output is valid JSON or equals the
  input, and valid input is returned byte-identical.
