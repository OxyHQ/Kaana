# Native admission cutover

This is an operator runbook for the reviewed internal metered population. Source
support does not mean a policy, database migration or authority cutover has run.
Kaana's commercial ledger and merchant activation are separate gates; this
internal population has no customer charge or invoice.

## Immutable inputs

Review the exact application owner, application, environment, application class,
internal flag and live workload credential roster returned by Oxy's existing
workload resolver. A service-token lane alone is insufficient: a secret-backed
service credential or newly issued handle must not inherit enrollment. Historical
retired handles receive permanent replay tombstones but no new execution rights.

A versioned `configs/native-admission/*.json` policy file is packaged in the image
at `/etc/kaana-native`. The task definition sets `KAANA_NATIVE_POLICY_PATH` and
`KAANA_NATIVE_POLICY_SHA256` to the same reviewed file and exact byte hash on every
task. An absent pair disables native admission. A partial pair or changed file
refuses startup. Policies contain opaque identity IDs and reviewed constraints,
never credentials. The immutable policy digest is separately reviewed and checked
against its database registration. Changing a limit, roster or economic treatment
requires a new policy version; stale server plans fail at admission.

The initial legacy Alia workload continues its verified internal metered terms:
32 concurrent requests, 5,000 per UTC day, controlled input ceiling 126,976;
ordinary text USD 0.05, image-input USD 0.15 with at most 16 images, explicitly
named promotional Pro/Ultra USD 1.00 with XHigh fallback; speech 15,000 characters
and USD 0.25; transcription 300,000 ms and USD 0.05; realtime qualification USD
0.25/minute. Exact deployment/model/provider tuples come from the reviewed prior
policy. Realtime admission also quotes the complete bounded session and response
fees, rather than treating a per-minute amount as a session hold.

A `live` session (Kaana ADR 0012) is admitted by kind like any other realtime kind,
and a line priced only by its clock (`session_milliseconds`, with `requests` at most
at the session's maxResponses and no other unit above zero; GPT-Live) is quoted by
that clock alone, mirroring Oxy's clock-only session plan. The reviewed policy
`configs/native-admission/alia-kaana-v2.json` does not list `live` among its realtime
kinds: serving `live` natively is a follow-up that needs a reviewed v3 policy
registration, not an edit of v2.

The new output ceiling of 8,192 is the reviewed implementation choice addressing
observed reasoning exhaustion under the former 2,048 clamp. It can increase actual
usage within unchanged monetary caps; the catalogue's price/context bounds still
qualify each request. As in the previous edge, a positive caller allowance is
normalized to `min(requested, policy ceiling)` before auto classification, quotation
and execution. Backend defaults of 16,384 become 8,192; utility requests of 2,560
remain 2,560. Nonpositive allowances are refused. It is not evidence that the user selected a token amount.
Promotional availability is checked against local current credential economics;
execution retains the existing free → credit → paid ordering. Only promotional
steps impose the existing credit ceiling, including prepaid within that ceiling.
Ordinary text and voice preserve the absence of a funding ceiling, including
unknown funding after known tiers. Unknown or unavailable promotional evidence
cannot admit that step. No provider secret or Oxy economic authorization is needed
for this local check.

## Initial policy evidence

The reviewed activation candidate is `configs/native-admission/alia-kaana-v2.json`,
policy `alia-kaana-native` version 2. Its file SHA256 is
`eb3385f238ff5639771542b0caeb6cb7474347d59f205400351e6f9c2adcf490`;
its canonical policy digest is
`e3a93c71c530c37dbd545c8a0ce4111ba17c467739513a2a4b5b5a66a136b382`.
It contains only the independently verified API workload handle. The integrations
workload, secret-backed service credentials and other principals receive no new
rights. The application/environment authority boundary still refuses them without
calling Oxy's economic endpoints. Registration and configuration do not activate
this policy until the reviewed fence import completes.

Version 1 remains packaged solely as historical evidence of the inactive
foundation release; never register or activate it. Its file SHA256 remains
`682ee01191981e4ceaff0ec163061ef99ac02b8598c34977da18ce293b56bfa2`.
Version 2 adds the verified deployment-scoped customer pricebook without changing
the reviewed identity roster, monetary limits or global catalogue prices.

The 2026-11-13 read-only probes used the deployed Oxy source
`b8294d61f5f44b9f7fdc9cfb343bd93a55b3f2ff` for the effective-policy double read.
The private principal artifact SHA256 is
`dec5b62496e182dd4cac62c4e0d0689efd5c02b4bb8315d8a4a7eb6c8bb62c36`;
the effective-policy artifact SHA256 is
`bb68585cece783281509cc5773b32833488eb0be64a571202895257ed31e617c`.
Both probes confirmed their deployed image and stopped successfully without
inference or financial writes. The effective routing policy is absent, all seven
power levels have profiles, allowed-profile restrictions and default target are
absent, same-model failover is enabled, and Console capacity overrides are null.
Provider/region/license lists are empty; ZDR, no-training, commercial-use-rights
and hosted-only requirements are false; BYOK and dedicated capacity are disabled.
The native policy preserves these facts rather than adding privacy or funding
restrictions. Its exact routes retain Kaana's reviewed commercial declarations;
missing declarations and enterprise/BYOK-only routes are refused before admission.

The packaged policy is tested against the reviewed migrated tariff: 126,976 input
units plus the normalized 8,192 output allowance on the reviewed DeepSeek 0731
route quotes USD 0.007968358400, below USD 0.05. Lower monetary ceilings and an
insufficient context window still refuse it. These are deterministic price and
admission checks, not live provider inference or proof of deployment.

### Scoped tariff evidence

The read-only deployed Oxy export has SHA256
`317618cf04035449b23a43d438e2adbb60b528400cf6237f4fa8e2cde61ba7f3`.
All eight exact deployment/model/provider tuples matched one approved source
snapshot. Version 2 preserves their exact amounts, currencies and units in its
own immutable price versions, with the source version and evidence reference.
The three GPT-OSS providers have different tariffs; they are never collapsed into
one model-wide price. DeepSeek's inherited price also differs from Kaana's global
catalogue, which remains unchanged for other populations.

Each route must independently fit the request ceiling. Tariff variants in the
same power-level step retain the existing funding/preference/failover order.
Settlement and commercial receipt validation select only the actually served
provider, deployment and immutable model reference from durable usage. Missing
or inconsistent served identity is unpriced reconciliation, never an inferred
allocation. Cache-write permission uses that same tuple; no provider borrows
another provider's cache tariff. The authenticated catalogue view shows a scoped
price for a single-tariff line and omits an aggregate price for provider-dependent
lines. It does not mutate the shared catalogue.

Tests cover the eight exported schedules, real adapter and route capabilities,
8192-token budget bounds, different-tariff failover, caller price preferences,
cache-write isolation and durable settlement replay. Production activation still
requires the deployed identity and database-role canaries, the complete app/env
fence/drain/export and reviewed version-2 import described below.

## Database authority and artifact

The image includes `kaana-native`. Provision `kaana_admission_runtime` and
`kaana_admission_admin` as NOLOGIN group roles before migration. The reviewed
migration login must be able to `SET ROLE kaana_migrator`, create the admission
schema and migration ledger, and own the existing provider-cost insertion
function. The admission runtime has only its schema DML, read-only policy/fence
access and EXECUTE on the provider-cost recording function. It cannot edit policy,
enrollment, tombstones, or provider credentials. The operator login can SET ROLE
`kaana_admission_admin`; no serving login may belong to that admin role.

Grant the serving login membership in `kaana_admission_runtime`. The server opens
a separate verified-TLS pool and immediately SET ROLEs to that role; no schema or
enrollment is changed at startup. Validate the actual runtime/admin/migrator
memberships and successful SET ROLE under the deployed secret bindings before
activation. The packaged `kaana-native inspect-access` reads only PostgreSQL
catalog metadata with a read-only startup setting and transaction; it reports
actual role existence, membership, SET ROLE authority, schema permissions and
provider-cost function ownership. Missing roles are evidence for provisioning,
not permission to create them from the serving process. Keep connection strings
and private roster artifacts out of logs.

Run the candidate's `kaana-native migrate` as the migration operator for the
initial bootstrap, after the reviewed roles exist. Subsequent normal deployments
inspect both the live and candidate native configuration before schema preparation.
If either requires native support, the existing credential-migrator task runs
`kaana-credentials migrate --native`: credential migrations first, then native
migrations under `kaana_migrator`, using the existing protected migration DSN.
Without native configuration it still runs only `kaana-credentials migrate`.
A migration failure prevents serving updates. Configuration appearing after the
schema step also prevents the update, rather than serving against an unprepared
schema. This task never registers a policy, imports a fence or enables enrollment.

Run
`kaana-native register --policy-file PATH --policy-file-sha256 HASH --policy-id ID
--policy-version VERSION --policy-sha256 DIGEST` as the enrollment operator.
Registration creates a disabled enrollment; it does not disable an existing
active enrollment. Verify the actual state with the same reviewed policy flags:
`kaana-native inspect --policy-file PATH --policy-file-sha256 HASH --policy-id ID
--policy-version VERSION --policy-sha256 DIGEST`. This fixed-query inspector uses
the existing migration operator DSN and `kaana_migrator` role, a read-only
connection and one repeatable-read/read-only transaction. It checks every stored
migration checksum against the packaged migrations, and the selected registered
policy digest and document against the reviewed policy. Corruption or a conflicting
policy is an error; missing/incomplete migrations and absent enrollment or policy
are reported explicitly. Output contains only migration state, registration and
enrollment-presence booleans, enabled/fence-complete flags, and active policy
version/digest. No principal, replay key, request row or policy document is emitted.
It does not repair, register, disable or activate anything.

Before configuration/cutover, require `migrationState: "verified"`,
`enrollmentPresent: true`, `registered: true`, `enabled: false`,
`activePolicy: null` and `legacyFenceComplete: false`. After the reviewed import,
require enabled/fence-complete and the expected active version/digest. All these
commands use the operator's `DATABASE_URL` from the normal protected secret
channel and print bounded metadata, not database errors or credentials.

## Ordered activation

1. Keep Gemies on its compatible pre-cutover transport until native activation.
   The old public grant lane refuses the backend's ordinary 16,384-token default;
   deploying direct transport against that lane is not a compatible staging step.
   Prepare the reviewed direct-transport image for the bounded maintenance window.
2. Deploy native-capable Kaana code with native configuration absent. Verify the
   exact fleet image and identity-only endpoint from Kaana's real workload. That
   canary verifies a signed identity assertion without provider execution,
   reservation, settlement or billing writes. Run the packaged
   `kaana-native identity-check --application-id REVIEWED_KAANA_APPLICATION_ID`
   from that exact workload with the existing canonical Oxy origin, production
   environment and public verification keys. Success prints only
   `{"verified":true}`; it does not open a database or execute inference.
3. Apply the candidate schema and register the reviewed immutable policy while
   its enrollment remains disabled. Prove role boundaries and migration digest.
4. Deploy the reviewed policy PATH+SHA to the whole fleet while the database
   enrollment stays disabled. New tasks deliberately refuse the selected
   population during this bounded window; older tasks may still use Oxy until
   the fleet converges. Before fencing, removing configuration can restore the
   old authority if a prerequisite fails. Never enable native admission here.
5. Confirm the exact steady fleet, then activate Oxy's permanent admission fence
   for the entire reviewed application/environment. Native policy still authorizes
   only its explicit credential subset; a different owner, handle or credential
   lane cannot escape to Oxy under that application/environment. Drain all old
   admitted grants and holds in the fenced scope.
   Existing old settlement remains Oxy-owned. Export the canonical manifest only
   when unresolved admissions, grants and holds are all zero.
6. Independently review the exported manifest hash, fence ID, exact activatedAt,
   live credential roster, policy digest and population. Manifest version 2 includes
   `dailyUsage` for `application_environment`: the export UTC day, all admitted
   requests in that day, and the positive effective daily limit. The policy limit
   must equal that reviewed limit. Eligible roster entries do not automatically
   become authorized; historical aliases remain fenced even for retired handles.
   Keep the artifact in a
   private location. `kaana-native validate` accepts the same review flags as
   import and reads the manifest on stdin or from `--manifest-file PATH`; it
   performs no database operation. For ECS, download the independently reviewed
   private S3 object into a shared ephemeral volume with a separately reviewed
   sidecar. Grant only that exact object's read permission, require encrypted
   storage, pin the downloader image, and require its successful exit before the
   native container starts. The downloader must set file ownership to the native
   container's UID 65532 and mode 0600; mount the volume read-only in that container.
   Never put the artifact, credentials or a presigned URL in argv or overrides.
   Test this exact delivery path with a synthetic fixture and `validate` in the
   candidate image before fencing, then validate the actual export before import.
   The CLI rejects nonregular, symlinked, nonprivate or larger-than-64-MiB files
   before opening the database. The reviewed canonical manifest hash is supplied
   independently; downloading an artifact or calculating its hash is not approval.
7. Run `kaana-native import` with the registration flags plus
   `--manifest-sha256 HASH --fence-id ID --activated-at EXACT_TIMESTAMP`, with the
   manifest on stdin or the same reviewed `--manifest-file PATH`. One transaction verifies and inserts every permanent old
   key/request alias, carries the legacy daily consumption, records immutable
   import evidence, and activates the exact
   policy pointer. Any mismatch or write failure rolls back the whole operation.
8. Deploy Gemies' reviewed direct transport only after native activation, then
   verify exact live source/image/config hashes, native generation records,
   all Gemies text and voice operations, completion replay after process loss,
   policy/capacity refusal, and no old financial authorization or settlement for
   new native admissions. A failure never retries inference through Oxy.

After Oxy fencing, recovery is forward-only: do not unfence, delete tombstones or
route failed native requests to the old owner. Unpriced or excessive actual usage
is reconciliation evidence, never invented zero usage. A successful terminal is
released only after content-free completion evidence is durable; the native
replay worker resumes settlement on startup without re-executing inference.

## Roster refresh and historical reads

A new live handle needs a newly reviewed immutable policy version and a refreshed
Oxy manifest under the **same permanent fence ID and activatedAt**. A changed
manifest hash requires a strictly higher version of the same policy ID and a
nondecreasing export time. Every previous tombstone must remain present and
unchanged. Imports retain immutable audit records. Omission, rollback or a new
fence epoch refuses activation. New imports require a snapshot for the current
PostgreSQL UTC day. Daily consumption has a separate legacy component: refresh
adds only its nonnegative increase to the combined counter, preserving native
admissions. Consumption above the reviewed limit is retained and blocks new
admissions until the next UTC day. The enrollment row lock serializes import
and admission. An exact completed replay for the active policy is a read-only
acknowledgement, including after midnight; it cannot reset quota or re-enable a
disabled enrollment. Migration `0002` adds this accounting without changing the
published `0001` checksum.

Manifest epoch/export timestamps must use Oxy's exact `Date.toISOString()`
representation: UTC with three fractional millisecond digits. Noncanonical
precision or offsets are refused even when the reviewed hash matches, so
PostgreSQL cannot change an accepted epoch through timestamp rounding.

The importer uses Oxy's canonical serializer semantics: UTF-16 key ordering,
JSON.stringify escaping, and no HTML/U+2028 rewriting. Real cross-language fixtures
cover opaque keys with emoji, HTML characters and Unicode separators. Unknown old
body digests are permanent tombstones, not fabricated hashes. No TTL permits
re-execution of an old key.

Generation reads authenticate the original principal and attribution. A known
native ID/key never queries Oxy, including pending or foreign-owned records. Only
confirmed native absence can perform the existing authenticated historical Oxy
receipt read. Database/authentication/parse failures do not select that read path;
it cannot authorize, reserve, settle or charge.
