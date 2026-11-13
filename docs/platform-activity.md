# Anonymous ecosystem activity

The opt-in producer sends the real Kaana HTTP request and response directions
to Oxy's authenticated `/internal/activity` collector. It follows Oxy's own
collector (`packages/telemetry/src/collector.ts`, `packages/core/src/server/traffic.ts`):
where the two differ, Oxy's is the reference and the difference is stated below.

There is no separate enable flag: setting `KAANA_INFRASTRUCTURE_LONGITUDE` and
`KAANA_INFRASTRUCTURE_LATITUDE` is what turns it on, in every binary that
reports activity (`kaana`, `kaana-publisher`, `kaana-credential-control`,
`kaana-platform-credential-control`) — those coordinates feed nothing else, so
their presence is already an unambiguous signal. Incoming requests become
internal only after Kaana's existing Ed25519 verifier accepts the signed
envelope. Unverified requests cannot claim an internal source service.
Cloudflare PoP codes describe ingress infrastructure; client IP addresses,
paths, query strings, prompts, completions, request IDs, accounts, sessions and
provider credentials never enter an aggregate. A flow is regions, a service
name, scope, direction and type, plus a count.

## What is observed

- **Inbound** requests, by middleware on each binary's listener. A response
  direction is recorded only when the handler wrote one.
- **Provider calls** (`kaana`, and the `kaana-publisher` discovery client), by
  `Transport`: external, type `ai`. A response is recorded only when one
  exists, including failures with HTTP responses; a connection failure has only
  an outbound observation. Provider geography is unknown unless measured
  separately: the producer never guesses a data-centre location from a hostname
  or address, and the unlocated end is **omitted** from the aggregate (Oxy's
  schema makes both regions optional; Oxy's collector omits them too).
- **Calls to Oxy** (`kaana`: the grant and settlement delivery), by
  `OxyTransport`: internal flows to `oxy-api`. The destination region is the
  peer's own `X-Oxy-Region` answer when it is a well-formed region, otherwise
  omitted; Kaana's region travels to Oxy in `X-Oxy-Source-Region`. The type
  follows the path classifier below, and the path is read, never kept. Oxy
  token minting and the activity collector's own posts are not wrapped, so the
  collector cannot observe itself.

The source service of a verified inbound call is `oxy-api`. Unlike Oxy's
collector, which reads the service name of the authenticated service principal,
Kaana's verifier proves only a signature by an Oxy-held key id: the envelope
carries no service name, and Oxy is the only signer of every route that marks a
request verified, so there is no other value to read.

## Exclusions and type

One table (`classify.go`) decides what is never reported, for inbound and
outbound alike: first path segments `health`, `healthz`, `ready`, `readyz`,
`live`, `livez`, `status`, `platform-stats`, `platform-activity`,
`platform-infrastructure`, `infra-status` (each also under `/api/`), and the
prefixes `/internal/activity`, `/internal/v1/health`,
`/auth/service-token` and `/cdn-cgi/`. It is a superset of Oxy's list; a test
proves each excluded path produces no flow and that near-misses (`/statuspage`)
do.

The type comes from the first path segment after `internal`, `kaana`, `api` and
a version segment, as in Oxy's classifier, with Kaana's surface in the table:
inference routes (`chat`, `messages`, `responses`, `embeddings`, `rerank`,
`inference`, `models`, `realtime`, ...) are `ai`; `audio` and `images` are
`media`; `identities` is `identity`; everything else (credential control,
settlements, Oxy's `/internal/kaana` control routes) is `platform`. Oxy's own
table would call `/v1/messages` `communication`, which is wrong for an
inference API. Provider calls are always `ai`.

## Bounds, backoff and readiness

The in-memory collector holds at most 512 distinct flows (Oxy's bound) and
flushes every two seconds in chunks of at most 256, the collector's batch
limit. A failed chunk is discarded with the rest of that flush: there is no
durable activity history, retry queue or blocking of inference.

A failed publish backs off as Oxy's does. After the Nth consecutive failure the
next attempt waits half of `min(10 min, 2 s * 2^(N-1))` plus up to half again at
random, and a success resets it. While waiting, flushes and heartbeats are
skipped and the collector keeps aggregating, bounded; the final flush and the
removal at shutdown are always attempted. The first failure of a streak and the
recovery are each logged once, with a fixed message carrying no credential or
request detail.

The heartbeat reports `online` only while the process's readiness function says
it can serve, else `unknown`. `kaana` is ready while its inventory snapshot is
loaded and inside the staleness horizon; the publisher and the two
credential-control binaries have no snapshot and are ready once their listener
is open (`Run` starts then).

## Deployment

`AWS_REGION`, `KAANA_INFRASTRUCTURE_LABEL`, `KAANA_INFRASTRUCTURE_LONGITUDE` and
`KAANA_INFRASTRUCTURE_LATITUDE` describe this deployment and belong in
oxy-infra; the longitude and latitude are also what enables reporting.
Registration starts after the listener opens. A ten-second heartbeat updates
the central inventory; graceful shutdown flushes traffic and then removes the
same random instance identity. An abrupt termination expires through the
central registry's lease. The central service broadcasts inventory changes to
the dashboard over its existing socket namespace.

Authentication reuses Kaana's dedicated Oxy service principal and its existing
short-lived token cache; no extra provider credentials or inference envelope
fields are introduced. Deploy the authenticated collector first, verify the
Kaana principal is admitted, and then set the task definition's coordinates.
Each binary reports under its own service name, so the publisher and the
credential-control processes appear as their own instances, not as serving
`kaana`.
