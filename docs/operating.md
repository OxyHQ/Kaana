# Running and deploying Kaana

Kaana refuses partial configuration. There is no unauthenticated local mode and
there is no provider-key fallback outside its database.

## Runtime configuration

| Variable | Required | Meaning |
|---|---|---|
| `KAANA_INVENTORY_PATH` | yes | deployment inventory snapshot |
| `KAANA_EDGE_PUBLIC_KEYS` | yes | `kid:base64,…` Ed25519 public keys; not secret |
| `KAANA_PROVIDERS` | yes | provider slugs served by this process |
| `DATABASE_URL` | yes | TLS PostgreSQL URL for Kaana's credential database |
| `KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN` | yes | symmetric KMS key ARN; not secret |
| `KAANA_OXY_SERVICE_API_KEY` | yes | public id of Kaana's dedicated Oxy service credential |
| `KAANA_OXY_SERVICE_API_SECRET` | yes | secret for that Oxy service credential; must carry only `inference:byok:validate` under the trusted Kaana application |
| `KAANA_OXY_SERVICE_ENVIRONMENT` | no | environment of that Oxy service credential, default `production`; verdicts for another environment are refused locally |
| `KAANA_OXY_API_BASE_URL` | no | Oxy API origin, default and exact deployed value `https://api.oxy.so`; only `development` may use an explicit loopback origin |
| `KAANA_OXY_VALIDATION_QUEUE_SIZE` | no | bounded off-request callback queue, default `256` |
| `KAANA_OXY_VALIDATION_TIMEOUT` | no | timeout per token/verdict HTTP operation, default `5s` |
| `KAANA_CREDENTIAL_VALIDATION_PROBE_TIMEOUT` | no | deadline for the isolated one-token upstream bootstrap probe, default `20s`, maximum `45s` so it cannot outlive its PostgreSQL lease |
| `KAANA_PROVIDER_RATES_PATH` | no | upstream rate cards; absent means cost is not measured |
| `KAANA_INVENTORY_MAX_AGE` | no | staleness horizon, default `1h` |
| `KAANA_INVENTORY_RELOAD_INTERVAL` | no | default `30s` |
| `KAANA_CREDENTIAL_RELOAD_INTERVAL` | no | atomic database/KMS pool reload, default `1m` |
| `KAANA_BREAKER_FAILURES_TO_OPEN` | no | default `3` |
| `KAANA_BREAKER_COOLDOWN` | no | default `5s` |
| `KAANA_BREAKER_MAX_COOLDOWN` | no | default `2m` |
| `KAANA_BREAKER_SUCCESSES_TO_CLOSE` | no | default `1` |
| `KAANA_ADDR` | no | default `:8080` |
| `KAANA_EDGE_MAX_SKEW` | no | default `5m` |
| `KAANA_MAX_ENVELOPE_BYTES` | no | default `16777216` |

Failover has no process-wide switch. The signed request's ordered
`authorizedRoutes` list is the complete authority: an absent list narrows a
concrete target to its declared primary, and a routing-profile target without a
list is refused.

Per provider, `<SLUG>` is upper-cased and `.`/`-` become `_`:

| Variable | Required | Meaning |
|---|---|---|
| `KAANA_PROVIDER_<SLUG>_PROTOCOL` | for an unknown slug | `openai_compatible` or `anthropic_messages` |
| `KAANA_PROVIDER_<SLUG>_BASE_URL` | for an unknown or account-scoped slug | non-secret provider API root |
| `KAANA_PROVIDER_<SLUG>_REGIONS` | no | upstream execution/residency regions, comma-separated; never Kaana's AWS region |
| `KAANA_PROVIDER_<SLUG>_KEY_RETIREMENT` | no | retired-key window, default `15m` |
| `KAANA_PROVIDER_<SLUG>_KEYS_ON_SEPARATE_ACCOUNTS` | no | whether a throttle may rotate accounts |

No variable contains a provider key. Public attribution metadata is compiled
into the reviewed provider configuration; adapters apply authentication from
the decrypted pool at send time.

Twenty-four providers have protocol and global API-root defaults: `openai`, `anthropic`,
`openrouter`, `cerebras`, `groq`, `xai`, `mistral`, `deepseek`, `sambanova`,
`siliconflow`, `ai21`, `google`, `together`, `cohere`, `fireworks`, `hyperbolic`,
`digitalocean`, `nvidia`, `modelscope`, `zai`, `nebius`, `nscale`, `chutes` and
`ovhcloud`. Alibaba Model Studio and Cloudflare Workers AI carry built-in
protocol and endpoint-identity rules, but no default root: their official URLs
contain a workspace/region or account id, so `_BASE_URL` remains explicit.
Protocols are a closed list because a binary can only construct adapters it
contains; provider slugs are not. A built-in serving configuration does not
imply discovery or publication support.

Two slugs that collapse onto one environment prefix are refused. For example,
`open-router` and `open.router` would both read `KAANA_PROVIDER_OPEN_ROUTER_*`;
silently choosing one would configure a provider under another identity.

Provider model-list endpoints do not report execution/residency regions. The
publisher copies `_REGIONS` into each discovered deployment only when an
operator can back the declaration with the upstream provider's own terms. When
it is absent, the deployment carries no regional attestation. It may match an
explicitly empty `authorizedRoutes.regions` set, which Oxy emits only when the
effective policy has neither `allowedRegions` nor `deniedRegions`; any signed
non-empty set is refused by exact inventory comparison. `AWS_REGION` describes
where Kaana runs and is never a substitute.

Example, with no secret material:

```bash
KAANA_PROVIDERS=cerebras,openrouter
KAANA_PROVIDER_OPENROUTER_KEYS_ON_SEPARATE_ACCOUNTS=true
DATABASE_URL='postgres://kaana:…@postgres.internal.oxy.so:5432/kaana?sslmode=verify-full&sslrootcert=/etc/ssl/certs/aws-rds-global-bundle.pem'
KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN='arn:aws:kms:us-west-2:…:key/…'
```

Provider metadata headers are reviewed constants in the binary. Any
`KAANA_PROVIDER_<SLUG>_HEADERS` value is refused, including a public-looking
one, so neither a header value nor a provider base URL can become a second
environment transport for a provider credential.

## Credential database

`provider_credentials` is the only durable home for a platform-operated
upstream key. The row contains provider/key identity, KMS ciphertext, KMS key
ARN, pool order, class, optional budget metadata and lifecycle state.
PostgreSQL never receives plaintext.

Customer BYOK keys live beside the platform pools in
`customer_provider_credentials`, never in Oxy, Vault, SSM, Secrets Manager or
environment. Their KMS context binds `provider + ownerAccountId + connectionId
+ environment + credentialHandle + revision`. See
`customer-provider-credentials.md`; its separate operation ledger contains
signed request identity, terminal outcomes and a write-only SHA-256 secret
fingerprint for changed-secret replay refusal, and that task split is separate
from the operator pool commands below.

The inference task opens the customer resolver over that same PostgreSQL/KMS
boundary. It receives only an exact signed authorized-route binding, decrypts
only that generation into a request-scoped one-key pool, and clears the returned
byte slice after copying it into the provider call path; the request pool drops
its retained copy immediately when the adapter returns. Its database role can
execute only the exact active-row function and its KMS role needs `Decrypt`, not
`Encrypt`. A resolution failure never falls back to the platform pool. The
decrypted value must be visible ASCII valid for an HTTP credential header, and
BYOK routes are isolated from the platform deployment breaker's admission and
outcome state. A second breaker/throttle lane is keyed by the complete customer
credential generation, so one tenant or revision cannot suppress another and a
non-attributable BYOK failure never fails over to an unbound route.

Successful normal BYOK use and explicit authentication refusal enqueue a
closed-vocabulary validation verdict back to Oxy. Billing/credit refusal is not
an invalid-key verdict; the same generation remains revalidatable after top-up.
Throttles and provider outages do not disable the connection.

Initial validation uses the distinct signed
`/internal/v1/customer-provider-credentials/validations` path and its durable
PostgreSQL lease/outcome ledger. It binds the exact application, provider,
owner, environment, connection, generation and deployment, makes a fixed
one-token provider call, discards output, and bypasses normal response, receipt,
Oxy billing, failover and breaker paths. Its full exact terminal outcome is sent
through the same narrow Oxy service principal to the bootstrap callback. A
pending Oxy operation can be reposted after either process restarts; Kaana
replays a terminal outcome without repeating provider spend.

The credential-control task requires a verified-TLS `DATABASE_URL` for its
dedicated database login, `KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN`, and
`KAANA_CREDENTIAL_CONTROL_PUBLIC_KEYS`. Its optional
`KAANA_CREDENTIAL_CONTROL_ADDR` defaults to `:8081`. It must not inherit a
provider key, the inference signing-key set, or KMS decrypt permission.

The KMS encryption context is:

```text
kaana:provider = <provider slug>
kaana:key-id   = <operator key id>
```

Both values are authenticated by KMS. Copying ciphertext from one row to
another therefore makes decryption fail instead of moving authority between
providers or key identities.

The runtime role needs only:

- `SELECT` on the `active_provider_credentials` view, never the base table that
  retains disabled history;
- `kms:Decrypt` on the one configured key;
- network access to PostgreSQL and KMS.

It does not need `INSERT`, `UPDATE`, DDL or `kms:Encrypt`. The publisher has the
same read/decrypt need because it authenticates one `GET /models` request. An
operator one-shot task owns migration/write/encrypt authority and is not a
long-running service.

Ordinary put/disable mutations call one `SECURITY DEFINER` database function
that changes the credential and appends `provider_credential_audit` in the same
statement. Rekey/deduplicate use a prepare/complete function pair inside one
explicit transaction whose advisory and table locks remain held across the KMS
operation. The credential-admin role has no direct table DML and reads metadata
through projections that exclude ciphertext; it cannot change a row without its
audit/operation record or forge either. Those records contain the session
database principal, a required `KAANA_CREDENTIAL_ACTOR`, the action and exact
provider/key identity, but neither plaintext nor ciphertext.

`DATABASE_URL` is still a secret connection credential and remains in the ECS
task's secret block. It is not an upstream provider key. It must use
`sslmode=verify-full` with a trusted root. Encryption without hostname
verification, and every plaintext or unverified fallback, are startup errors.
The release image pins AWS's global RDS CA bundle by SHA-256 at build time and
installs it at `/etc/ssl/certs/aws-rds-global-bundle.pem`.

### Create or migrate the schema

Run with a migration database role, not the serving role:

```bash
kaana-credentials migrate
```

The migration is idempotent. Runtime never applies DDL at startup; granting a
serving process schema authority to make deployment convenient would make every
request-serving task a migration principal.

### Add or rotate a key

Plaintext is accepted only on standard input:

```bash
<secret-manager-read-command> | kaana-credentials put \
  --provider openrouter \
  --key-id 123e4567-e89b-42d3-a456-426614174000 \
  --position 1 \
  --class paid
```

Set `KAANA_CREDENTIAL_ACTOR` to the non-secret operator or automation identity
for `put`, `import-ssm` and `disable`; a mutation without one is refused.

The source command must write the value only to its stdout. The CLI has no value
flag and no provider-secret environment variable, so the key cannot land in
argv, shell history, a task definition or a GitHub Actions environment.

For the one-time removal of legacy SSM SecureStrings, use the direct importer:

```bash
kaana-credentials import-ssm \
  --parameter /oxy/alia/PROVIDER_KEY_OPENROUTER \
  --provider openrouter \
  --key-id legacy-alia-20260901 \
  --position 1 \
  --class paid
```

The importer requests decryption through the AWS SDK, requires the exact
historical parameter/provider/key-id triple, immediately re-encrypts
the value under Kaana's KMS key, and never writes it to stdout, argv, an
environment variable, or a file. It accepts only `SecureString` values under
the four exact historical Alia handoff paths for ElevenLabs, Groq, OpenRouter
and xAI plus four exact historical Relay paths for Cerebras, Groq, OpenRouter
and xAI. It accepts no prefix, wildcard or other provider. Every path is bound
to its exact provider slug, and the task role narrows that code allow-list
further to the exact parameters being migrated. It exists only for migration;
after a verified
provider call, delete the legacy parameter and the corresponding GitHub secret.

Each Relay handoff uses the same command, with its bound provider identity stated
explicitly and no value crossing the command line. For example:

```bash
kaana-credentials import-ssm \
  --parameter /oxy/relay/RELAY_PROVIDER_CEREBRAS_API_KEY \
  --provider cerebras \
  --key-id cerebras-relay-main \
  --position 1 \
  --class paid
```

Deploy the importer code and its exact IAM source grant before running that
one-shot. Verify the resulting non-secret row metadata, then deploy publisher
and serving configuration with Cerebras enabled. A successful real request
through Kaana is the gate before removing the legacy source; an import alone is
not proof that the account can invoke a model.

`put` accepts only an exact lowercase UUIDv4 key ID. Putting an existing
`(provider, keyId)` encrypts new plaintext and atomically rotates that row.
Adding another key is another opaque row and does not register a task
definition. The legacy importer is the only named-ID exception and only for its
eight frozen historical handoffs. Serving reloads the complete set atomically every
`KAANA_CREDENTIAL_RELOAD_INTERVAL`: a partial or failed load leaves the previous
generation serving. Revoke the old credential upstream first when immediate
revocation matters; a database disable converges within the configured interval.

List only non-secret metadata:

```bash
kaana-credentials list
```

Disable without deleting ciphertext or losing the operator identity:

```bash
kaana-credentials disable --provider openrouter --key-id 123e4567-e89b-42d3-a456-426614174000
```

Historical platform key IDs are replaced only through `rekey-id`, never by
copying ciphertext or reading plaintext into a shell. Exact duplicate checks use
`deduplicate`; PostgreSQL receives only `deduplicated` or `different`, never a
secret fingerprint. The frozen IDs, idempotency receipts, exact commands and
dedupe-before-rekey order are in
[`provider-credential-id-cutover.md`](provider-credential-id-cutover.md).

Never print a source key to pipe it. Verify a real provider call through the new
row, then delete the old SSM/GitHub copy and prove task definitions no longer
reference it.

## Publisher configuration

`cmd/kaana-publisher` reads the same non-secret provider configuration and the
same encrypted database pools as the serving process. It uses the one active
credential selected by an exact, non-secret PostgreSQL key id for the complete
catalogue traversal; serving owns pool order and rotation.

| Variable | Required | Meaning |
|---|---|---|
| `KAANA_PROVIDERS` | yes | serving superset used to prove that discovery cannot publish an unroutable provider |
| `KAANA_DISCOVERY_PROVIDERS` | yes | slugs to discover; an ordered subsequence of the serving task's `KAANA_PROVIDERS` |
| `KAANA_PROVIDER_<SLUG>_DISCOVERY_KEY_ID` | for every discovery slug | exact enabled PostgreSQL key id used for model discovery; no first/position fallback |
| `DATABASE_URL` | yes | credential database, TLS required |
| `KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN` | yes | expected KMS key ARN |
| `KAANA_INVENTORY_BUCKET` | yes | S3 bucket; never defaulted |
| `KAANA_INVENTORY_KEY` | yes | object key |
| `AWS_REGION` | yes | AWS region |
| `KAANA_PUBLISH_INTERVAL` | no | default `15m` |
| `KAANA_PUBLISHER_ATTRIBUTION_PATH` | no | checked-in attribution table |

A provider declared in either process's provider set but missing an active
database key is a hard startup refusal for that process. A green task must not
advertise an adapter or discovery target that cannot authenticate. Publisher
startup requires both variables, proves that every discovery slug is present in
the serving superset and keeps the same priority order.

Serving and publisher both reload credentials atomically. A failed database or
KMS read leaves the previous complete generation in use and is logged; no
request or discovery cycle observes a pool assembled from two rotations.

Discovery is provider-specific even when serving uses the shared Chat
Completions adapter. Mistral publishes capability flags and only
`completion_chat` models are accepted. SiliconFlow is queried with
`type=text&sub_type=chat`. SambaNova uses the documented OpenAI-compatible
list. Nebius requests verbose model metadata and drops `-fast` delivery
flavours; every other id still needs an exact attribution. Alibaba uses its
native authenticated `/api/v1/models` catalogue, paginates the declared total,
asks only for `TG` inference rows and retains only exact text-output ids; only
four separately attributed dated snapshots may publish. Cloudflare remains
absent from `KAANA_DISCOVERY_PROVIDERS` because its current official
model-search schema leaves result rows untyped. Nscale documents an
organization-scoped OpenAI list.
Only three fixed Meta ids from their official examples are attributed. An
account capture must still prove entitlement and task before that allow-list
can grow. Attribution then accepts only independently verified, fixed chat
identities. Moving aliases and preview ids are dropped.

DeepSeek is serving-compatible but its current direct catalogue exposes moving
aliases, and AI21 publishes no account model-list endpoint. Neither is eligible
for automatic publication until Kaana has a reviewed catalogue control.
Alibaba instead uses its documented authenticated native catalogue and can
publish only the exact dated snapshots present in the attribution allow-list.
A generic protocol match is never treated as proof that a model exists for the
account.

Chutes and OVHcloud have built-in serving origins but are marked
`not_available` for publication: their documented catalogues are public and do
not establish what one Kaana credential may invoke. Hugging Face, Kilo, LLM7
and OpenCode Zen remain explicit configuration because their endpoints are
routers; a downstream-provider selector is not a direct provider deployment.

The publisher's S3 permission stays separate from serving. Writing the
inventory decides routing, so a request-serving role must not acquire it merely
because the two binaries ship in one image.

## Container and deployment

The image contains four static binaries:

- `/usr/local/bin/kaana` serves inference;
- `/usr/local/bin/kaana-publisher` publishes inventory;
- `/usr/local/bin/kaana-credentials` is for one-shot administration.
- `/usr/local/bin/kaana-credential-control` accepts signed customer-BYOK
  create/rotate/revoke mutations under an encrypt-only task role.

It runs distroless as uid 65532 with no shell or package manager. CI pushes one
digest and updates serving and publisher from that same digest. Provider keys
are never synchronized by CI and never exist as repository secrets.

The inventory is mounted at `/etc/kaana/inventory.json`, not baked into the
image. A baked snapshot becomes stale on a clock while the container remains
green. Kaana reloads an atomically replaced file and keeps the last good
snapshot after a failed reload.

`GET /livez` is the only unsigned route and the load-balancer health path.
`/internal/v1/health` requires a signed envelope; `/health` does not exist. The
distroless image cannot run a shell-based Docker/ECS health command, so the
probe comes from outside the container.

The Cloudflare DNS workflow never retires the legacy `api.kaana.ai` name by
default. Retirement requires `action=apply`,
`retire_legacy_api_dns=true`, the expected dedicated `alb_dns`, and
`legacy_api_target=oxy-alb-648111691.us-west-2.elb.amazonaws.com` exactly. In
that mode it re-reads the exact proxied apex CNAME, requires
`https://kaana.ai/livez` to identify a healthy Kaana response, then requires
exactly one DNS-only `CNAME` named `api.kaana.ai` whose content byte-matches
that reviewed old shared Oxy ALB target. Only that record id is deleted, and
its absence is read back. Whitespace, a missing or duplicate record, another
record type, a proxied record, another target, or any drifted prerequisite
refuses the deletion before a mutation.

The edge public key belongs in plain environment. Kaana can verify with it and
cannot sign an envelope it would accept. The signing private key stays in Oxy.

### Cloudflare request ceiling

Public traffic to `kaana.ai` reaches Cloudflare before Kaana's dedicated load
balancer. Oxy's normal edge-to-data-plane traffic resolves `kaana.ai` through
the private Route 53 zone, so it does not consume this public counter.

`.github/workflows/cloudflare-rate-limit.yml` owns one zone-level
`http_ratelimit` rule, identified by the stable ref
`kaana_public_inference_per_ip`. It applies only to
`/internal/v1/inference`, counts by Cloudflare data center and source IP, and
blocks after 20 requests in 10 seconds for 10 seconds. This is an origin-abuse
ceiling, not a customer quota or Kaana routing policy.

Run the workflow with `action=check` first. Check mode performs GET requests
only and fails on missing or drifted state. `action=apply` additionally requires
`confirm_apply=kaana.ai`; it creates or updates only the rule with Kaana's ref,
never deletes another rule, then reads the entry point back. A duplicate ref or
a manual rule using Kaana's managed description is a hard conflict rather than
an invitation to guess which rule to overwrite. DNS remains owned by the
separate Cloudflare DNS workflow.

The existing Actions secret `CLOUDFLARE_API_TOKEN` supplies the token. It is
currently organization-scoped with `visibility=all`, so the public Kaana
repository receives it; this workflow does not rely on an organization secret
restricted away from private repositories. For this workflow the token needs
Zone Read and Zone WAF Edit, scoped only to `kaana.ai`. If the same token
continues to serve the DNS workflow, its zone-scoped DNS Edit permission remains
necessary too. The workflow names that one secret directly; it never enumerates
the repository secret context.

## Provider lifecycle

Adding a provider:

1. verify its official endpoint, protocol, terms and stable model IDs;
2. run adapter conformance and a real canary;
3. insert its encrypted database key;
4. add its non-secret configuration and task IAM/network access;
5. restart Kaana and publisher;
6. attribute verified upstream IDs and publish the inventory.

Retiring one reverses the serving dependency:

1. remove its deployments from the published inventory;
2. remove it from both `KAANA_PROVIDERS` and `KAANA_DISCOVERY_PROVIDERS` where present, then deploy;
3. disable/revoke its database credentials;
4. revoke the provider-side key.

Deleting or disabling a key before routes stop using it converts a controlled
retirement into a full startup failure or refused customer request.

## Validation gates

```bash
go build ./...
go vet ./...
go test -race ./...
golangci-lint run ./...
cd tools/contract && bun install --frozen-lockfile && bun run generate && bun run validate
```

Also verify in production:

- task definitions contain `DATABASE_URL` and the KMS ARN but no provider key;
- the task role can decrypt only the Kaana KMS key and cannot encrypt;
- the database contains ciphertext, never plaintext;
- a row-swapped ciphertext fails KMS context authentication;
- serving and publisher use the same image digest and endpoint facts; publisher
  startup has proved its explicit provider set is an ordered subsequence of
  serving;
- signed health reports every declared provider configured;
- old GitHub secrets, SSM parameters and task-definition revisions are retired.
