# ADR 0013 — Image generation is served only behind Kaana's own content check

- Status: accepted for the serving precondition, the handler order and the
  gates (the owner's instruction of 2026-11-17). The classifier that implements
  the check is **not decided**: it needs the owner decisions listed under "What
  enabling image generation needs", and until they are made no process serves
  the route.
- Date: 2026-11-17
- Contract: no change. No route, field or error code is added.
- Companions: [ADR 0007](0007-image-input.md) (an image is customer content and
  Kaana fetches no image URL), [ADR 0002](0002-kaana-public-api-and-sdk.md)
  (the declared catalogue is Kaana's alone), [ADR 0010](0010-kaana-sells-and-invoices.md)
  (who holds money).

## Context

`POST /v1/images/generations` (contract set 8.12.0) validated the shape of a
request and nothing about its content. Whatever a provider's own filter let
through reached the caller. Image tools that produced sexualised images of real
people and of minors have put their operators in court; a provider's filter is
that provider's product decision and changes without notice, so it cannot be the
only thing between a prompt and such an image on a service Oxy answers for.

Five facts decide what can be built now.

1. **Nothing serves the route today.** No deployment in `configs/inventory.json`
   could generate an image: only the `openai` slug's adapter executes
   `images_generations` (`providerconfig.ExecutableAPIFormats`) and the snapshot
   carries no `openai` image model. A barrier added now breaks no caller.
2. **Kaana already holds the safety facts.** A declared model's `safety` member
   (`modelSafetyMetadataSchema`: `contentFilteringDefault` of `none`,
   `provider_default` or `strict`; `provenanceMarking` of `none`,
   `visible_watermark`, `invisible_watermark` or `c2pa`) is in
   `configs/public-catalogue.json`, which is Kaana's alone and which Oxy reads
   back. Nothing has to cross the Oxy–Kaana boundary for Kaana to read it at
   request time. Six lines declare the member today, every one
   `provider_default` / `none`, and none of them is an image line.
3. **The declared member was never validated.** `ParsePublicCatalogue` carried
   it as raw JSON, so a misspelt word loaded without complaint.
4. **Classifier models exist in the inventory, but no way to call one.**
   `meta-llama/llama-guard-4-12b` and `nvidia/nemotron-3.5-content-safety` are
   declared `platform_internal` with image and text input and have OpenRouter
   deployments; `openai/gpt-oss-safeguard-20b` takes text only; the Llama Prompt
   Guard ids are refused under every adapter
   ([provider-onboarding-log](../provider-onboarding-log.md)). But every
   execution in Kaana runs on an authority a caller brought: a grant Oxy signed
   for the caller's credential, a native admission for the caller's verified
   principal, or a signed envelope. Kaana has no principal of its own, no grant
   it can ask for on its own behalf and no account a classifier call could be
   metered to. `AGENTS.md` forbids attempting a route no admission, envelope or
   grant authorized, and puts anything that moves money under "ask first".
5. **The signed lane would bypass a check in the public handler.**
   `POST /internal/v1/inference` accepted an `images_generations` envelope and
   handed it to the executor, although the lane streams events and has no
   response shape for an image.

## Decision

1. **Image generation is served on a model reference only when all three
   hold** (`Server.servesImages`), each read before Oxy is asked, so a request
   on a line that fails one holds no money:
   - this process has Kaana's own content check (`Config.ImageSafety`);
   - the declared catalogue states, for the line,
     `safety.contentFilteringDefault: strict` and a `safety.provenanceMarking`
     other than `none` (`PublicCatalogue.DeclaresImageGenerationSafety`);
   - some deployment of the line could generate an image (`servesFormat`).

   A line that fails one is refused `unsupported_modality` naming `model`, as a
   line nothing can draw already was. On an image line, `strict` states that
   every prompt and every generated image passes Kaana's own check;
   `provider_default` states that only the provider's filter applies, which is
   not enough to serve.
2. **The check runs twice and fails closed both times** (`handleImages`):
   - on the prompt, after field validation and before `admit`: a prompt that is
     not allowed asks Oxy for nothing and reaches no provider;
   - on the generated images, after the executor answered and before anything
     is written to the caller.

   Only an explicit `ImageSafetyAllowed` without an error lets the request
   continue. The zero verdict is "no answer". A refusal by the check is
   `policy_violation` (403, not retryable), naming `prompt` when the prompt was
   refused. An error, no answer or an unknown verdict is `service_unavailable`
   (503, retryable): the same request may pass once the check answers. Neither
   message names what the check found, and the check's own error is never
   logged, because it may quote what it was asked about.
3. **The check is a port, `publicapi.ImageSafety`, and it has no
   implementation.** `cmd/kaana` wires none, so by decision 1 nothing is served.
   This is deliberate: the interface fixes what the handler guarantees, and the
   classifier behind it waits for the decisions below instead of being built on
   an authority nobody granted.
4. **A caller with no credential is refused before the prompt is checked.**
   `admit` would refuse the request anyway, and the check must not be free to
   call.
5. **The signed envelope lane serves no image generation.** An envelope whose
   `client.apiFormat` is `images_generations`, whose `modality` is `image` or
   which carries `image` controls is refused `invalid_request` before any
   adapter runs, as a decisions or rerank envelope is. The public route is the
   only way in, and it is the one that runs the check.
6. **The declared `safety` member is held to the contract's vocabulary** when
   the catalogue loads: an unknown member, a missing word or a word outside
   either enum stops the load.
7. **`configs/inventory.json` carries no image-generating deployment on a line
   the catalogue does not declare safe**, and today carries none at all
   (`TestNoCheckedInDeploymentGeneratesImagesWithoutDeclaredSafety`, exact
   count, with a positive control). The published snapshot a serving task loads
   is held by decision 1 at request time, not by this test.
8. **No contract change.** `policy_violation` and `service_unavailable` are in
   the public vocabulary. `policy_violation`'s public description becomes "your
   account policy or Kaana's content policy forbids this request"; a client
   that must tell the two apart reads `param` and the message. A dedicated code
   would be a contract change and was not made.

## What enabling image generation needs

All of these, each reviewed, before a line declares `strict`:

- **An authority and a meter for the classifier call (owner decision).** The
  options found, none of which exists today:
  - *Oxy grants Kaana's own workload.* Kaana presents its workload identity
    (Oxy ADR 0026) and Oxy signs an `internal_metered` grant for a
    `platform_internal` classifier line. It needs Kaana in Oxy's first-party
    registry as an inference consumer, an economic relationship in Oxy for it,
    and a grant operation the classifier call fits.
  - *A native admission for Kaana's own principal* (ADR 0010): a reviewed
    `internal_metered` policy in `configs/native-admission/` naming that
    principal and the classifier lines. It needs the same identity, verified
    through the identity lane.
  - *The caller's image grant carries the classifier steps*, and Oxy prices the
    check into the image. The prompt check then cannot precede the grant, so a
    refused prompt releases a hold instead of never taking one; it changes the
    grant and decision 2's order.
  - Rejected: calling a provider's moderation endpoint outside the executor. It
    spends a pool key on a route nothing authorized and meters nothing.
- **The classifier and its vocabulary, from primary evidence.** The check must
  refuse, at the least, sexual content involving minors and sexualised or nude
  depictions of real, identifiable people. It uses the categories the chosen
  classifier natively reports, cited from its model card in the change that
  adds it. Where the classifier has no category for "an identifiable person",
  the broader native category (all sexual content) is refused; a category is
  never invented in a prompt and trusted.
- **Where customer content goes.** The prompt and the images reach the
  classifier's provider, which the caller's grant did not admit. That provider
  must be inside zero retention, and the regions the caller's grant allows must
  be honoured or the request refused.
- **Who may make the check run.** The prompt check precedes the grant, and on
  the grant lane the grant is also the authentication. Decision 4 only requires
  that a credential was presented. A paid check needs the caller identified
  first (the identity lane) or its own rate limit.
- **Inline output.** Kaana fetches no image URL (ADR 0007). An image delivered
  as a URL has bytes Kaana never held, so an implementation cannot judge it and
  must answer with an error. A route is enabled only when its adapter obtains
  the image inline from the provider whatever `response_format` the caller
  asked for.
- **The provenance marking, from primary evidence** that the provider stamps
  it or that Kaana does. `none` is the truthful declaration until then.
- **What a withheld image costs.** The provider drew and billed it, and today
  the settlement carries its units. Whether the caller pays for an image they
  never received is Oxy's price to decide.

## Consequences

- The route exists and answers `unsupported_modality` for every model, as it
  did before this ADR for want of a deployment. It now does so for a second,
  independent reason, and adding a deployment no longer opens it.
- A future implementation changes no handler: it implements the port, is wired
  in `cmd/kaana`, and the catalogue line is declared.
- Oxy's edge can no longer send image generation on the signed lane. Nothing
  did.
- A catalogue file with a malformed `safety` member, which used to load, stops
  the process at boot. The checked-in file's six members are well-formed.

## Rules a reviewer applies

- A change that serves an image without `ImageSafety` having allowed both the
  prompt and the images, or that treats an error or a missing answer as
  allowed, is refused. `image_safety_test.go` mutation-tests each of these.
- `strict` or a provenance marking is declared on an image line only with the
  evidence above cited in the commit, never to make a deployment servable.
- An `ImageSafety` implementation runs its classifier through the executor on
  an authority this ADR's open decision granted. One that calls a provider any
  other way, or that persists or logs a prompt or an image, is refused.
- The exact count in `TestNoCheckedInDeploymentGeneratesImagesWithoutDeclaredSafety`
  is raised only in the change that enables image generation.
- Image generation is not added back to the signed lane without the same check
  running there.
