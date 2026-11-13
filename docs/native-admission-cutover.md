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

The new output ceiling of 8,192 is the reviewed implementation choice addressing
observed reasoning exhaustion under the former 2,048 clamp. It can increase actual
usage within unchanged monetary caps; the catalogue's price/context bounds still
qualify each request. It is not evidence that the user selected a token amount.
Promotional availability is checked against local current credential economics;
execution retains the existing free → credit → paid ordering and credit ceiling,
including prepaid within that ceiling. Unknown or unavailable promotional evidence
cannot admit that step. No provider secret or Oxy economic authorization is needed
for this local check.

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
activation. Keep connection strings and private roster artifacts out of logs.

Run the candidate's `kaana-native migrate` as the migration operator. Run
`kaana-native register --policy-file PATH --policy-file-sha256 HASH --policy-id ID
--policy-version VERSION --policy-sha256 DIGEST` as the enrollment operator.
Registration alone leaves the enrollment disabled. All these commands use the
operator's `DATABASE_URL` from the normal protected secret channel. They print
bounded status/digests, not database errors or credentials.

## Ordered activation

1. Deploy Gemies' direct Kaana transport first and verify its exact served image.
   Before this cutover Kaana still uses the old content-free financial grant lane.
2. Deploy native-capable Kaana code with native configuration absent. Verify the
   exact fleet image and identity-only endpoint from Kaana's real workload. That
   canary verifies a signed identity assertion without provider execution,
   reservation, settlement or billing writes.
3. Apply the candidate schema and register the reviewed immutable policy while
   its enrollment remains disabled. Prove role boundaries and migration digest.
4. Deploy the reviewed policy PATH+SHA to the whole fleet while the database
   enrollment stays disabled. New tasks deliberately refuse the selected
   population during this bounded window; older tasks may still use Oxy until
   the fleet converges. Before fencing, removing configuration can restore the
   old authority if a prerequisite fails. Never enable native admission here.
5. Confirm the exact steady fleet, then activate Oxy's permanent admission fence
   for the exact workload population. Drain old admitted grants and holds.
   Existing old settlement remains Oxy-owned. Export the canonical manifest only
   when unresolved admissions, grants and holds are all zero.
6. Independently review the exported manifest hash, fence ID, exact activatedAt,
   live credential roster, policy digest and population. Keep the artifact in a
   private location. `kaana-native validate` accepts the same review flags as
   import and reads the manifest on stdin; it performs no database operation.
7. Run `kaana-native import` with the registration flags plus
   `--manifest-sha256 HASH --fence-id ID --activated-at EXACT_TIMESTAMP`, with the
   manifest on stdin. One transaction verifies and inserts every permanent old
   key/request alias, records immutable import evidence, and activates the exact
   policy pointer. Any mismatch or write failure rolls back the whole operation.
8. Verify exact live source/image/config hashes, native generation records,
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
fence epoch refuses activation; an exact replay is idempotent.

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
