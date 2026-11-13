# ADR 0011 — A batch API is out of scope: Kaana is synchronous and stateless

- Status: accepted (a scope statement; it changes no code and no contract)
- Date: 2026-11-13
- Companion: [ADR 0009](0009-contract-gaps-found-comparing-with-openrouter.md)
  compared Kaana with OpenRouter and left batches out. Oxy's side of the
  reservation protocol is Oxy's ADR 0009 (`docs/adr/0009-usage-reservation-and-settlement.md`
  in the oxy repo), which is a different document from Kaana's.

## Context

A batch API is asynchronous job submission: the caller uploads a file of many
requests, the provider works through it inside a completion window of about 24
hours at roughly half the price, and the caller polls for the result file.
OpenAI offers it, and OpenRouter lists a `POST /batches` surface. A customer
comparing Kaana with either can reasonably ask for it, and the contract already
carries a `batches` name in `APIFormat`.

Three facts about Kaana and Oxy decide the question.

1. **Kaana is a synchronous, stateless data plane.** It executes one signed,
   already-authorized request and answers it. Its PostgreSQL holds the
   credential store and the settlement outbox and nothing else
   ([architecture](../architecture.md)). It has no job table, no queue and no
   result store.
2. **A batch needs the one thing Kaana is forbidden to keep.** `AGENTS.md` and
   [rules](../rules.md) forbid persisting or logging a prompt (images included)
   or a completion. A batch, run by Kaana, is a stored input file, a queue of
   pending lines and a stored output file for the caller to collect hours later:
   prompts and completions at rest.
3. **Oxy's ledger cannot hold a reservation across 24 hours.** Oxy reserves
   spend, then settles once, inside `RESERVATION_TTL_SECONDS` (900 seconds in
   `packages/api/src/services/inferenceEdge.service.ts`), and expires what is
   left. Oxy records why it serves no `POST /v1/batches` in
   `packages/api/src/routes/inferenceEdge.ts` (the comment beside the routes
   that exist) and in `docs/architecture/inference-responsibility-matrix.md`:
   a 24-hour hold would be released mid-batch and the work would later settle
   against a reservation that no longer stands. Raising the TTL is not the fix,
   because a day-long hold on a shared balance is a different financial
   product; Oxy's ADR 0009 assumes exactly one settlement per hold, so a batch
   needs an amendment to it, not an endpoint. The ceiling is also a sum over
   every line's own ceiling and no sounder than the weakest modality inside.

Kaana has no public batch route today, and no adapter lists `batches` in
`providerconfig.ExecutableAPIFormats`. `TestExecutableAPIFormatsMatchWhatEachAdapterTranslates`
pins those lists exactly, so no provider can execute the name, and the publisher
refuses to attach a model to an adapter that cannot execute the family. The
name is therefore already unreachable and needs no new refusal.

## Decision

1. **Kaana does not offer a batch API.** There is no route, no job store, no
   queue and no stored input or output file. A request that names the `batches`
   family cannot be routed: no adapter executes it.
2. **`batches` stays in the `APIFormat` enum as a reserved name.** It is not
   removed (the enum is a shared, closed contract and removing a value is a
   breaking change for no benefit) and it is not added to
   `grantOperationSchema`, so no authorization can grant it. Reserved means
   spoken for, not served.
3. **No contract change and no version bump.** This ADR is documentation.
4. **What would have to be true to revisit.** All of these, not any one:
   - **A provider-held batch pass-through.** The batch file lives with the
     provider (the provider's own file and batch objects), Kaana forwards
     creation, status and retrieval, and Kaana stores no prompt or completion
     at any point. Whether a provider's retention terms are acceptable for a
     customer's data is its own review.
   - **A long-lived reservation class in Oxy**, with its own expiry and partial
     settlement (a hold per sub-request at dispatch, or one hold settled in
     parts), which is an amendment to Oxy's ADR 0009 made in the oxy repo first.
   - **An opaque, Oxy-signed job handle** that Kaana can verify and the caller
     can poll with, so Kaana keeps no job state of its own and an Oxy id is
     never parsed or joined on.
   - **An owner decision** that the product is wanted, since it widens the
     contract (a new route, request and response shapes, a grant operation) and
     "ask first" applies.

## Consequences

- A customer who needs 24-hour discounted bulk work sends ordinary requests
  from their own queue; Kaana adds no discount for it.
- The reserved enum name carries no promise. Nothing should add `batches` to a
  grant, an adapter list or a catalogue line until the conditions above hold.
- Oxy and Kaana keep one answer to the question: Oxy serves no `/v1/batches`
  route, and Kaana executes no `batches` request.

## Rules a reviewer applies

- A change that stores a prompt, a completion or an input or output file to
  support asynchronous work is refused, whatever it is called.
- `batches` appears in no `ExecutableAPIFormats` result and no
  `grantOperationSchema`. The exact-list test above is the control: a diff that
  makes it fail for `batches` is this ADR being reversed and needs the owner.
- A reservation or settlement change that stretches the hold past Oxy's TTL is
  reviewed in Oxy first; Kaana does not work around it.
- Do not remove `batches` from the enum to "clean up"; it is reserved.
