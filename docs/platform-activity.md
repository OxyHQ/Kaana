# Anonymous ecosystem activity

The opt-in `OXY_ECOSYSTEM_ACTIVITY_ENABLED=true` producer sends the real Kaana
HTTP request and response directions to Oxy's authenticated `/internal/activity`
collector. Incoming requests become internal only after Kaana's existing
Ed25519 verifier accepts the signed envelope. Unverified requests cannot claim
an internal source service. Cloudflare PoP codes describe ingress infrastructure;
client IP addresses, prompts, completions, request IDs and provider credentials
never enter an aggregate.

Upstream provider requests are observed by each adapter's HTTP transport. A
response is recorded only when a response exists, including failures with HTTP
responses. A connection failure has only an outbound observation. Provider
geography is unknown unless measured separately; the producer never guesses a
provider's data-centre location from a hostname or IP address. Unknown locations
remain countable but cannot be drawn as a geographic route.

The bounded in-memory collector combines at most 256 flow kinds and flushes
every two seconds. Collector failure drops that batch rather than building a
durable activity history or blocking inference. Publisher errors carry a fixed
message without credential or request details.

`AWS_REGION`, `KAANA_INFRASTRUCTURE_LABEL`,
`KAANA_INFRASTRUCTURE_LONGITUDE` and `KAANA_INFRASTRUCTURE_LATITUDE` describe
this deployment. They are required when enabled and belong in oxy-infra.
Registration starts after the listener opens. A ten-second heartbeat updates
the central inventory; graceful shutdown flushes traffic and then removes the
same random instance identity. An abrupt termination expires through the
central registry's lease. The central service broadcasts inventory changes to
the dashboard over its existing socket namespace.

Authentication reuses Kaana's dedicated Oxy service principal and its existing
short-lived token cache; no extra provider credentials or inference envelope
fields are introduced. Deploy the authenticated collector first, verify the
Kaana principal is admitted, and then enable the task definition flag. The
producer covers the inference-serving process and provider adapters; publisher
and credential-administration processes have their own lifecycle and are not
reported as serving instances.
