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
| `KAANA_PROVIDER_<SLUG>_BASE_URL` | for an unknown slug | provider API root |
| `KAANA_PROVIDER_<SLUG>_REGIONS` | no | upstream execution/residency regions, comma-separated; never Kaana's AWS region |
| `KAANA_PROVIDER_<SLUG>_KEY_RETIREMENT` | no | retired-key window, default `15m` |
| `KAANA_PROVIDER_<SLUG>_KEYS_ON_SEPARATE_ACCOUNTS` | no | whether a throttle may rotate accounts |

No variable contains a provider key. Public attribution metadata is compiled
into the reviewed provider configuration; adapters apply authentication from
the decrypted pool at send time.

Twenty-four providers have protocol and API-root defaults: `openai`, `anthropic`,
`openrouter`, `cerebras`, `groq`, `xai`, `mistral`, `deepseek`, `sambanova`,
`siliconflow`, `ai21`, `google`, `together`, `cohere`, `fireworks`, `hyperbolic`,
`digitalocean`, `nvidia`, `modelscope`, `zai`, `nebius`, `nscale`, `chutes` and
`ovhcloud`. Alibaba Model Studio remains explicit because its root contains the
account workspace and region. Protocols are a closed list because a binary can
only construct adapters it contains; provider slugs are not. A built-in serving
origin does not imply discovery or publication support.

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

`provider_credentials` is the only durable home for an upstream key. The row
contains provider/key identity, KMS ciphertext, KMS key ARN, pool order, class,
optional budget metadata and lifecycle state. PostgreSQL never receives
plaintext.

The KMS encryption context is:

```text
kaana:provider = <provider slug>
kaana:key-id   = <operator key id>
```

Both values are authenticated by KMS. Copying ciphertext from one row to
another therefore makes decryption fail instead of moving authority between
providers or key identities.

The runtime role needs only:

- `SELECT` on `provider_credentials`;
- `kms:Decrypt` on the one configured key;
- network access to PostgreSQL and KMS.

It does not need `INSERT`, `UPDATE`, DDL or `kms:Encrypt`. The publisher has the
same read/decrypt need because it authenticates one `GET /models` request. An
operator one-shot task owns migration/write/encrypt authority and is not a
long-running service.

Every mutation calls one `SECURITY DEFINER` database function that changes the
credential and appends `provider_credential_audit` in the same statement. The
credential-admin role has no direct table DML and reads metadata through a view
that excludes ciphertext; it cannot change a row without its audit or forge an
audit row. The audit contains the session database principal, a required
`KAANA_CREDENTIAL_ACTOR`, the action and provider/key identity, but neither
plaintext nor ciphertext.

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
  --key-id openrouter-main \
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
  --key-id openrouter-main \
  --position 1 \
  --class paid
```

The importer requests decryption through the AWS SDK, immediately re-encrypts
the value under Kaana's KMS key, and never writes it to stdout, argv, an
environment variable, or a file. It accepts only `SecureString` values under
the legacy `/oxy/alia/PROVIDER_KEY_*` handoff prefix plus the exact historical
Cerebras path `/oxy/relay/RELAY_PROVIDER_CEREBRAS_API_KEY`; other Relay paths
are refused. Its task role must narrow that code allow-list further to the exact
parameters being migrated. It exists only for migration; after a verified
provider call, delete the legacy parameter and the corresponding GitHub secret.

The Cerebras handoff uses the same command, with the source identity stated
explicitly and no value crossing the command line:

```bash
kaana-credentials import-ssm \
  --parameter /oxy/relay/RELAY_PROVIDER_CEREBRAS_API_KEY \
  --provider cerebras \
  --key-id cerebras-relay-main \
  --position 1
```

Deploy the importer code and its exact IAM source grant before running that
one-shot. Verify the resulting non-secret row metadata, then deploy publisher
and serving configuration with Cerebras enabled. A successful real request
through Kaana is the gate before removing the legacy source; an import alone is
not proof that the account can invoke a model.

Putting an existing `(provider, keyId)` encrypts new plaintext and atomically
rotates that row. Adding another key is another row and does not register a task
definition. Serving reloads the complete set atomically every
`KAANA_CREDENTIAL_RELOAD_INTERVAL`: a partial or failed load leaves the previous
generation serving. Revoke the old credential upstream first when immediate
revocation matters; a database disable converges within the configured interval.

List only non-secret metadata:

```bash
kaana-credentials list
```

Disable without deleting ciphertext or losing the operator identity:

```bash
kaana-credentials disable --provider openrouter --key-id openrouter-main
```

Never print a source key to pipe it. Verify a real provider call through the new
row, then delete the old SSM/GitHub copy and prove task definitions no longer
reference it.

## Publisher configuration

`cmd/kaana-publisher` reads the same non-secret provider configuration and the
same encrypted database pools as the serving process. It uses the first active
key for one unmetered discovery call; serving owns rotation.

| Variable | Required | Meaning |
|---|---|---|
| `KAANA_PROVIDERS` | yes | serving superset used to prove that discovery cannot publish an unroutable provider |
| `KAANA_DISCOVERY_PROVIDERS` | yes | slugs to discover; an ordered subsequence of the serving task's `KAANA_PROVIDERS` |
| `DATABASE_URL` | yes | credential database, TLS required |
| `KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN` | yes | expected KMS key ARN |
| `KAANA_INVENTORY_BUCKET` | yes | S3 bucket; never defaulted |
| `KAANA_INVENTORY_KEY` | yes | object key |
| `AWS_REGION` | yes | AWS region |
| `KAANA_PUBLISH_INTERVAL` | no | default `15m` |
| `KAANA_PUBLISHER_ATTRIBUTION_PATH` | no | checked-in attribution table |

A provider declared in either process's provider set but missing an active
database key is a hard startup refusal for that process. A green task must not
advertise an adapter or discovery target that cannot authenticate. The
publisher temporarily accepts `KAANA_PROVIDERS` as a compatibility fallback,
but new task definitions must set both variables explicitly. Publisher startup
proves that every discovery slug is present in the serving superset and keeps
the same priority order.

Serving and publisher both reload credentials atomically. A failed database or
KMS read leaves the previous complete generation in use and is logged; no
request or discovery cycle observes a pool assembled from two rotations.

Discovery is provider-specific even when serving uses the shared Chat
Completions adapter. Mistral publishes capability flags and only
`completion_chat` models are accepted. SiliconFlow is queried with
`type=text&sub_type=chat`. SambaNova uses the documented OpenAI-compatible
list. Nebius requests verbose model metadata and drops `-fast` delivery
flavours; every other id still needs an exact attribution. Nscale documents an
organization-scoped OpenAI list.
Only three fixed Meta ids from their official examples are attributed. An
account capture must still prove entitlement and task before that allow-list
can grow. Attribution then accepts only independently verified, fixed chat
identities. Moving aliases and preview ids are dropped.

DeepSeek is serving-compatible but its current direct catalog exposes moving
aliases; AI21 publishes no account model-list endpoint; Alibaba requires a
workspace/region URL and has no documented account model list. Those three are
not eligible for automatic publication until Kaana has a verified static
catalog control. A generic protocol match is never treated as proof that a
model exists for the account.

Chutes and OVHcloud have built-in serving origins but are marked
`not_available` for publication: their documented catalogues are public and do
not establish what one Kaana credential may invoke. Hugging Face, Kilo, LLM7
and OpenCode Zen remain explicit configuration because their endpoints are
routers; a downstream-provider selector is not a direct provider deployment.

The publisher's S3 permission stays separate from serving. Writing the
inventory decides routing, so a request-serving role must not acquire it merely
because the two binaries ship in one image.

## Container and deployment

The image contains three static binaries:

- `/usr/local/bin/kaana` serves inference;
- `/usr/local/bin/kaana-publisher` publishes inventory;
- `/usr/local/bin/kaana-credentials` is for one-shot administration.

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

The edge public key belongs in plain environment. Kaana can verify with it and
cannot sign an envelope it would accept. The signing private key stays in Oxy.

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
