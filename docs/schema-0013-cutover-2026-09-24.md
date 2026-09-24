# Schema 0013 serving cutover, 2026-09-24

This record covers the production cutover of the exact deployment-credential
runtime (schema 0013) and the xAI speech deployment that depends on it. The
general procedure is in [`operating.md`](operating.md#schema-0013-staged-rollout).
This page gives the exact identities and commands for this run. Nothing on it
is secret.

## State when prepared

| Fact | Value |
|---|---|
| Serving | `kaana` at `oxy-kaana:43`, image `sha256:7672df2b…` (Groq backport of `df1fa39`, no binding gate), desired 1 |
| Publisher | `kaana-publisher` at `oxy-kaana-publisher:47`, image `sha256:cd202c32…` (`df1fa39`, no speech discovery) |
| Serving providers | `cerebras,groq,xai,openrouter` (serving and discovery) |
| Schema | 0013 and 0014 applied. The migrator of `a1523152` ran from release run `35734587699` and printed `Kaana credential schema is current` |
| Admin pin | `a1523152d94b2d022a2e3b6acdbabb4cf393da7f` → index `sha256:6f62c8eed0bddb81648401a4e07e26f8d4027e988b6c132ea5743e87f215486c` (linux/arm64 + attestation) |
| Bindings in the database | 340 rows from `snap_dfd6904a99d6313b`, applied and verified 2026-09-11 (admin tasks `04583a19…`, `d84b9de0…`) |
| Live inventory | `snap_ebf19b144959bbb8`, 333 deployments, reissued every 15 minutes |
| Reviewed manifest | `configs/cutovers/production-bindings-snap_ebf19b144959bbb8.json`, S3 version `hZsP6lsuSFMehoKgem9HF6NHZr43_0bd` |
| Cutover variable | `KAANA_CREDENTIAL_RUNTIME_SCHEMA_0013_COMPLETE` is absent |

Manifest summary. Every served deployment has one active key binding:

| Provider | Key ID | Deployments |
|---|---|---|
| cerebras | `43405cea-a7d1-49c2-ba73-5a84536d3abf` | 1 |
| groq | `8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1` | 6 |
| openrouter | `b8090dce-82f2-4077-9fc1-fd831a53ca27` | 319 |
| xai | `1d72d527-81ca-41e5-9644-2d81a4b126ec` | 7 |

Of those 333 rows, 332 are already in the database with their `kdb_*` IDs. The
only new row is `dep_openrouter_z_ai_glm_5_2_free_observed_2026_09_01`, with ID
`kdb_00000000000000000000000000000341`. The 8 deployments the publisher withdrew
since `snap_dfd…` appear under `retainedBindings`.

`tts` is **not** in `snap_ebf19b144959bbb8`. The live publisher predates speech
discovery. xAI speech appears only after the new publisher runs and finds both
`eve` and `rex` in `GET /v1/tts/voices`. Its deployment ID will be
`dep_xai_tts_observed_<UTC date of that first publish>`, for example
`dep_xai_tts_observed_2026_09_24`, and its reference will be
`x-ai/text-to-speech@observed-<date>`.

No workflow in this rollout uses a GitHub environment. None of them waits for an
environment approval. Each one is a manual `workflow_dispatch` from `main`, by
someone who has write access to that repository.

## Runbook

Set `M` to the main commit that the PR carrying this page merges as. Run every
Kaana workflow on `--ref main`.

### 1. Merge and build

Merge the PR. The push runs `Deploy to AWS`. Because the variable is not set,
that run builds `oxy/kaana:$M` and runs the idempotent migrator one-shot. It
does not call `update-service`. Wait for the run to go green, then check the
image:

```bash
gh run list -R OxyHQ/Kaana --workflow deploy-aws.yml -L 1
aws ecr describe-images --repository-name oxy/kaana --image-ids imageTag=$M \
  --query 'imageDetails[0].imageDigest' --output text
```

The two batch binding operations run the image tagged with the dispatching
commit, because only that image bakes the new manifest. Dispatch them while `M`
is still main's head. If main moves to a commit that did not build an image,
first run `gh workflow run deploy-aws.yml -R OxyHQ/Kaana --ref main -f mode=build-only`.

### 2. Pre-flight readbacks (no mutation)

```bash
# the live object must still be snap_ebf19b144959bbb8
aws s3 cp s3://oxy-kaana-inventory-usw2-237343248947/inventory/current.json - | jq -r '.snapshotId, (.deployments|length)'
gh workflow run credential-admin.yml -R OxyHQ/Kaana --ref main -f operation=list
gh workflow run credential-admin.yml -R OxyHQ/Kaana --ref main -f operation=list-deployment-bindings
```

Required results:

- The live object reads `snap_ebf19b144959bbb8 333`. If the snapshot has
  moved, regenerate the manifest for the new object before you go on, by the
  same rules the second commit describes.
- `list` shows the four key IDs above as `enabled: true`.
- `list-deployment-bindings` returns 340 rows. Once the retained rows are
  removed from that set, the only manifest row it lacks is
  `dep_openrouter_z_ai_glm_5_2_free…`.

There is no dry-run mode. `verify-production-deployment-bindings` run now must
fail with exactly `binding readback has 340 rows, manifest requires 341`. That
failure is the dry-run proof.

### 3. Apply and verify

```bash
gh workflow run credential-admin.yml -R OxyHQ/Kaana --ref main -f operation=apply-production-deployment-bindings
gh workflow run credential-admin.yml -R OxyHQ/Kaana --ref main -f operation=verify-production-deployment-bindings
```

Both must print
`verified 333 exact deployment bindings for snap_ebf19b144959bbb8 (49534e10561d66eb651afb39860762e7c25abbe6064232d11532eef61361d017)`.
Apply issues exactly one database mutation, the new row. Re-running it is safe.

### 4. Signed readback of the live serving snapshot (Oxy)

Read the live oxy-api identity at execution time. When this page was written it
was `oxy-oxy-api:539` / `sha256:fc306d90…`.

```bash
TD=$(aws ecs describe-services --cluster oxy-cluster --services oxy-api --query 'services[0].taskDefinition' --output text)
IMG=$(aws ecs describe-task-definition --task-definition "$TD" --query "taskDefinition.containerDefinitions[?name=='oxy-api'].image|[0]" --output text); IMG=${IMG##*@}
gh workflow run kaana-signed-deployment-readback.yml -R OxyHQ/OxyHQServices --ref main \
  -f expected_live_task_definition_arn="$TD" -f expected_live_image_digest="$IMG" \
  -f reason="schema 0013 cutover: pre-deploy readback of snap_ebf19b144959bbb8"
```

The result must name `snap_ebf19b144959bbb8`, list 333 descriptors, and report
zero provider requests and zero ledger writes.

### 5. Isolated candidate plus one signed canary per key class

Candidate: `oxy/kaana:$M`, the image step 1 built, with its digest `D` from the
`describe-images` output there. It must be `M` and not the `a1523152`
administration pin: this PR changes the serving binding gate in `cmd/kaana`
(see "Unbound deployments" below), so `a1523152`'s serving source is no longer
the one that deploys.

```bash
gh workflow run candidate-canary.yml -R OxyHQ/Kaana --ref main \
  -f candidate_image_digest=<D> -f candidate_source_commit=$M \
  -f expected_snapshot_id=snap_ebf19b144959bbb8 -f hold_minutes=20
```

A candidate that fails the startup gate prints `startupError` and exits. Do not
go on until the job prints `candidateTaskArn`, `candidateTaskDefinitionArn`,
`candidateImageDigest` and `candidatePrivateIp`. The gate now refuses only an
effectively unpopulated binding table, so a start alone no longer proves every
deployment is bound. Step 3's exact verify is that proof. The candidate's log
must also carry no `deployments without an exact credential binding are
unroutable until bound` line. If it does, that line names the unbound IDs;
bind them before you go on. In the same 20-minute window,
run the Oxy canary once per class. Its concurrency group runs them one at a
time.

| Class | `deployment_id` | Status |
|---|---|---|
| cerebras / `43405cea…` | `dep_cerebras_gpt_oss_120b_observed_2026_09_01` | **Waived pending account funding.** See below |
| groq / `8295090b…` | `dep_groq_openai_gpt_oss_120b_observed_2026_09_01` | Required |
| xai / `1d72d527…` | `dep_xai_grok_4_3_observed_2026_09_01` | Required |
| openrouter / `b8090dce…` | `dep_openrouter_openai_gpt_4o_mini_observed_2026_09_01` (`kdb_…0181`) | Required |

The openrouter row is a paid deployment on the same key. The first candidate
run used the one new binding, `dep_openrouter_z_ai_glm_5_2_free…`, and
OpenRouter rate-limited that free model upstream. That result says nothing
about the key or the binding. The new row's binding is proved by step 3's
exact verify, not by a canary.

The Cerebras account has refused billing with a 402 on every chat completion
since 2026-09-03, so no receipt can show six passed cases. On that first run
its canary returned `provider_error` in 33 ms. That was a serving bug, not the
refusal: the bound view had no retirement policy, so the zero-length
retirement it recorded was refused by
`kaana_record_provider_credential_attempt` (fixed in
`fix(credentials): an exact key view retires with its pool's policy`). With
the fix, the candidate returns the provider's verdict. If you run this class
as a check that the refusal is reported correctly, the first request must fail
with `provider_billing_refused` (category `quota`, non-retryable). It records
`exhausted`, with `retired_until` one Cerebras retirement window after
`occurred_at`, in `provider_credential_runtime_state`. Any request inside that window
fails with `deployment_unavailable`, with no upstream call and with
`retryAfterMs` set to the key's return time. Do not treat that receipt as a
pass. Re-run the class as a real canary once the account is funded and the
retirement has lapsed.

```bash
gh workflow run kaana-signed-canary.yml -R OxyHQ/OxyHQServices --ref main \
  -f expected_live_task_definition_arn="$TD" -f expected_live_image_digest="$IMG" \
  -f expected_snapshot_id=snap_ebf19b144959bbb8 \
  -f candidate_task_arn=<candidateTaskArn> -f candidate_task_definition_arn=<candidateTaskDefinitionArn> \
  -f candidate_image_digest=<D> \
  -f candidate_private_ip=<candidatePrivateIp> -f deployment_id=<row above> \
  -f routing_profile_id=<opaque profile> -f routing_policy_id=<policy> -f routing_policy_version=<n> \
  -f account_id=<billing account> -f application_id=<application> -f credential_id=<credential> \
  -f confirm_two_provider_requests=true -f reason="schema 0013 cutover: <provider> key class"
```

The routing and billing identities are Oxy's reviewed canary identities, the
same kind the 2026-09-09 run `34302325992` used. Each receipt must show six
passed cases, two one-token provider requests and zero ledger writes. If the
window closes first, dispatch `candidate-canary.yml` again and continue.

### 6. Optional: pre-bind the speech deployment

Not needed while xAI holds exactly one enabled key. Count them in step 2's
`list`; the table above shows only the key each binding uses, not how many a
provider holds. A deployment with no exact binding then routes on its provider's
only key ([key-pools](key-pools.md#several-providers-and-a-key-pool-for-each)),
so `tts` is routable from its first snapshot. The explicit bind only matters if
a second xAI key is enabled before the close-out manifest. Run it only when
step 7 will start on the same UTC date as `<date>` here:

```bash
gh workflow run credential-admin.yml -R OxyHQ/Kaana --ref main -f operation=bind-deployment \
  -f binding_operation_id=kdb_00000000000000000000000000000342 \
  -f deployment_id=dep_xai_tts_observed_<YYYY_MM_DD> -f provider=xai \
  -f key_id=1d72d527-81ca-41e5-9644-2d81a4b126ec
```

A binding for a deployment that is not in the inventory has no effect. After
this step, `verify-production-deployment-bindings` reports 342 rows, not 341.
That is expected. The follow-up manifest for the speech snapshot restores the
exact verify.

### 7. Enable and deploy

```bash
gh variable set KAANA_CREDENTIAL_RUNTIME_SCHEMA_0013_COMPLETE -R OxyHQ/Kaana --body true
gh workflow run deploy-aws.yml -R OxyHQ/Kaana --ref main -f mode=deploy
```

This rebuilds `oxy/kaana:$M` and retags it; buildx attestations make the digest
differ from step 1. It then rolls the services in this order: `kaana`,
`kaana-publisher`, then `kaana-credential-control` and
`kaana-platform-credential-control` if they are ACTIVE. From here on, every
push to main deploys automatically. Confirm:

```bash
aws ecs describe-services --cluster oxy-cluster --services kaana kaana-publisher \
  --query 'services[].[serviceName,taskDefinition,deployments[0].rolloutState,runningCount]' --output table
```

Both services must run the job's printed `@sha256` and show `COMPLETED`. Then
repeat step 4 against the new serving task. It must still read
`snap_ebf19b144959bbb8` until the publisher changes the snapshot.

### 8. Speech discovery and the tts binding

Within about 1 minute of the publisher rollout, its first cycle publishes. Check
the result:

```bash
aws s3 cp s3://oxy-kaana-inventory-usw2-237343248947/inventory/current.json - \
  | jq -r '.snapshotId, ([.deployments[]|select(.provider=="xai")]|length), (.deployments[]|select(.upstreamModelId=="tts")|.deploymentId)'
```

Expect a new snapshot ID and 8 xAI deployments, the 7 grok models plus `tts`.

- **If xAI shows 0**, the voices call failed. Its error withdraws every xAI
  deployment from the snapshot, grok included. Serving accepts a withdrawal,
  so grok routes disappear. Roll the publisher back to `oxy-kaana-publisher:47`
  right away (see Rollback).
- **If `tts` is absent but grok is present**, xAI did not list both `eve` and
  `rex`. Speech is not published. Nothing is broken.
- **If `tts` is present**, it routes on xAI's only key with no bind. Confirm
  that the serving log carries no `deployments without an exact credential
  binding are unroutable until bound` line naming it. That line appears only
  if xAI holds two or more enabled keys. In that case, bind it:

  ```bash
  gh workflow run credential-admin.yml -R OxyHQ/Kaana --ref main -f operation=bind-deployment \
    -f binding_operation_id=kdb_00000000000000000000000000000343 \
    -f deployment_id=<the tts deploymentId> -f provider=xai -f key_id=1d72d527-81ca-41e5-9644-2d81a4b126ec
  ```

  There is no deadline. Until then a request routed to `tts` is refused and
  nothing is sent upstream; every other deployment serves. Serving picks the
  binding up on its next credential reload (≤1m), and the WARN stops on the
  inventory reload after that.

Serving installs the new snapshot on its next inventory reload (≤30s). Then
run step 4 once more. The
readback must name the new snapshot ID and include the `tts` descriptor.

### 9. Close out

- Add a reviewed successor manifest for the speech snapshot, with
  `snap_ebf19…`'s rows kept and `tts` as its new row. An explicit row is
  harmless next to the provider default, and it keeps `tts` routable if a
  second xAI key is ever enabled. Replace this manifest
  with it, so `verify-production-deployment-bindings` is exact again.
- Record the deploy run, final digests and readback run IDs in this file.

## Record of the run, 2026-09-24

All times UTC. `M` was first `c9f783d292e6f8f4fe43058a691ed9a635ba3319`
(PR #105), then `8e81466e2868146195e16c5b19012f0244084961` (PR #106, the
bound-view retirement fix). No provider held more than one enabled key at any
point, so step 6 was not run and no `bind-deployment` was issued.

| Step | Evidence |
|---|---|
| 1. Build `c9f783d2` | Release run `36009314420`: migrator one-shot only, `serving ECS services were not updated`. Candidate digest `sha256:371ec0c9661220d04540db29dc7e88629dc182cc811e02719c96a84fb7f50262` |
| 2. Pre-flight | Live object `snap_ebf19b144959bbb8 333`. `list` run `36009488017` (task `3dd2d245…`): the four keys enabled, one enabled key per served provider. `list-deployment-bindings` run `36009498135`: 340 rows, lacking only `kdb_…0341`. Verify run `36009918395` failed with `binding readback has 340 rows, manifest requires 341` |
| 3. Apply, verify | Runs `36010125216` and `36010329932` both printed `verified 333 exact deployment bindings for snap_ebf19b144959bbb8 (49534e10561d66eb651afb39860762e7c25abbe6064232d11532eef61361d017)` |
| 4. Pre-deploy readback | Oxy run `36010534014` against `oxy-oxy-api:539` / `sha256:fc306d90…`: `contract_version_mismatch`, zero provider requests, zero ledger writes. Oxy's workflow requires contract 3.0.0 and serving `oxy-kaana:43` reported 2.0.0, so this step cannot pass before the deploy. The owner deferred it to after step 7 |
| 5. Candidate `c9f783d2` | Candidates `6ad61f77…` (`oxy-kaana-candidate:4`) and `3fc780a1…` (`:5`), no unbound WARN. Oxy canaries: groq `36013749810` and xai `36013937136` passed; cerebras `36012165207` returned `provider_error` (the bound-view bug); openrouter on `dep_openrouter_z_ai_glm_5_2_free…` `36014167274` was rate-limited upstream |
| 1. Build `8e81466e` | Release run `36015997019`: `serving ECS services were not updated`. Candidate digest `sha256:b547e59716a02ead53b167c449249c7cc4240acc5b61eb16dee11c155ae28298` |
| 5. Candidate `8e81466e` | Run `36016287198`, task `4923197203424e11bd035e2b9af8a329` (`oxy-kaana-candidate:6`), no unbound WARN. Oxy canaries, each six passed cases, two one-token provider requests and zero ledger writes: groq `36016465230`, xai `36016703387`, openrouter `dep_openrouter_openai_gpt_4o_mini…` `36016938361`. Cerebras refusal check `36017185083`: `provider_billing_refused`, non-retryable (waived pending account funding) |
| 7. Enable, deploy | Variable set 15:05:40. `mode=deploy` run `36017611711`: `kaana` → `oxy-kaana:44`, `kaana-publisher` → `oxy-kaana-publisher:48`, both `sha256:add55174615a162d80f22d7ac354a4ca52df062b6145ca6fc111cb889c555d83`, `COMPLETED`. `kaana-credential-control` and `kaana-platform-credential-control` are not ACTIVE and were not rolled |
| 8. Speech | The first publisher cycle issued `snap_37548e4f1f8ec610` at 15:10:14.881, 334 deployments, 8 xAI, `tts` as `dep_xai_tts_observed_2026_09_24` (`x-ai/text-to-speech@observed-2026-09-24`). Serving installed it at 15:10:35 with no unbound WARN; `tts` routes on xAI's only key |
| 4. Post-deploy readback | Oxy run `36019806004` against `oxy-oxy-api:539` / `sha256:fc306d90…`: contract 3.0.0, `snap_37548e4f1f8ec610`, 334 descriptors including `dep_xai_tts_observed_2026_09_24`, zero provider requests, zero ledger writes |
| 9. Successor manifest | `configs/cutovers/production-bindings-snap_37548e4f1f8ec610.json` replaces this page's manifest. It pins S3 version `bB84mMzvBNUKtjKdmurcgBn5vIUCBNPJ` (the issue serving installed, sha256 `e73ea428…7c02`), keeps all 333 `snap_ebf19…` rows and the 8 retained rows verbatim, and adds `dep_xai_tts_observed_2026_09_24` on xAI key `1d72d527…` as `kdb_00000000000000000000000000000342` |

Until that manifest is applied, `verify-production-deployment-bindings` from a
build that bakes it fails with `binding readback has 341 rows, manifest requires
342`. Its apply issues exactly one mutation, the `tts` row, and must print
`verified 334 exact deployment bindings for snap_37548e4f1f8ec610
(e73ea428e95d0957e4773d0e89629450848289688aa0e31ef3474ea06e667c02)`. Dispatch
both from the merge commit while it is main's head, as in step 1.

## Rollback

The database schema and bindings stay as they are, because older binaries ignore
them.

```bash
gh variable set KAANA_CREDENTIAL_RUNTIME_SCHEMA_0013_COMPLETE -R OxyHQ/Kaana --body false
aws ecs update-service --cluster oxy-cluster --service kaana --task-definition oxy-kaana:43
aws ecs update-service --cluster oxy-cluster --service kaana-publisher --task-definition oxy-kaana-publisher:47
aws ecs wait services-stable --cluster oxy-cluster --services kaana kaana-publisher
```

Set the variable first, so a push to main cannot deploy again. Roll the
publisher back together with serving. The old publisher republishes without
`tts`, and it keeps every other first-seen date, because it reads them back
from the previous snapshot. If the credential-control services moved, return
them to their previous revisions as well. `describe-services` shows those
revisions before the deploy, so note them in step 7. A new serving task that
fails its startup gate during step 7 needs no rollback: the deployment circuit
breaker (rollback enabled, 50%) keeps `oxy-kaana:43` serving.

## Unbound deployments

A deployment with no exact binding routes on its provider's key when the
provider holds exactly one enabled key. It is unroutable when the provider
holds two or more keys. Step 2's `list` shows which providers have a default. The 333 explicit bindings stay: apply and
verify are unchanged, and an explicit row always wins over the default.

Serving refuses a snapshot, at startup and on every inventory reload, only
when **more than half** of the deployments it serves resolve to no key. That covers an empty or mostly empty binding table, and one that
points at disabled keys. It is what makes a bad release fail to start while
ECS keeps the previous revision.

Anything less is accepted and degrades per route. An unresolvable deployment is
refused at request time and never attempted, and Oxy moves to the next signed
route. Every inventory load (startup, and every 30s after) logs:

```text
WARN deployments without an exact credential binding are unroutable until bound
     unbound=<n> served=<m> deploymentIds=[…] providers=[…] snapshotId=…
```

The rule used to be "refuse if any served deployment is unbound". That froze
the inventory whenever the publisher discovered a model nobody had bound yet.
Worse, if the single serving task restarted (crash, host retirement, deploy)
while such a snapshot was mounted, the new task could not start, and with no
previous task beside it that meant a total inference outage. A per-provider
"zero bindings" rule would bring the same outage back for the first model of
every newly served provider, so the threshold is over all served
deployments. The publisher would have to more than double the served fleet
before anyone binds for ordinary discovery to reach it.

## Chat impact

`kaana` is one Fargate task behind the `oxy-kaana` target group. The service
does a rolling deploy with `minimumHealthyPercent=100` and `maximumPercent=200`,
and the circuit breaker rolls back. The new task starts next to the old one and
must pass `/livez` twice at 30-second intervals. Only then is the old task
deregistered, with a 60-second drain, SIGTERM and a 45-second `stopTimeout`,
during which the server finishes in-flight handlers.

- **Expected downtime:** none. Requests still in flight on the old task more
  than about 60 s after deregistration starts can be cut, for example very long
  streamed generations. A failed candidate never takes traffic.
- **Desired count 2:** not needed. The 100/200 settings already overlap two
  tasks during the roll, and the count belongs to Terraform. To cover long
  streams, run the deploy at low traffic rather than changing the target
  group's drain in the console.

## Risks

1. **Newly discovered deployments of a multi-key provider are unroutable until
   someone binds them.** A single-key provider's new models, `tts` included,
   route on its key with no bind. Adding a second key to a provider takes away
   that default for every deployment of it that has no exact row. It is a
   per-route degradation with no deadline. It neither freezes the inventory
   nor blocks a restart (see "Unbound deployments"). Before or right after the
   flip, add two alerts. One on the serving WARN `deployments without an
   exact credential binding are unroutable until bound`, which is the work
   queue for `bind-deployment`. One on `the inventory snapshot could not be
   reloaded; continuing to serve the last good one` whose error contains
   `startup credential binding gate`, which means the binding table looks
   unpopulated and the snapshot is frozen.
2. **xAI speech discovery sits in the grok discovery path.** One failure of
   `/v1/tts/voices` withdraws every xAI deployment for that cycle. Check it
   right after the publisher rolls, as in step 8.
3. **The candidate is not byte-identical to the deployment.** `mode=deploy`
   rebuilds. The canaried serving source is identical, but the digest is not.
   Confirm the digest ECS registered against the job output.
4. **The inventory can move before the flip.** Any publisher change between
   step 2 and step 7 invalidates the manifest. The new task still starts, and
   the new deployments are unroutable until bound (the WARN names them). The
   exact verify no longer matches, so regenerate the manifest and repeat steps
   3–5, or bind the new IDs and carry them in the close-out successor
   manifest.
5. **The credential readback is old.** The manifest cites the 2026-09-13 `list`,
   so step 2 repeats it. The database also refuses a disabled or missing key on
   its own.
6. **Contract version.** The new serving build reports
   `contractVersion 3.0.0`; the old one reports 2.0.0. It still accepts request
   envelope versions 1 and 2. Steps 4 and 5 are what show that Oxy's live client
   works with it.
