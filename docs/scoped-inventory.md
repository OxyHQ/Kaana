# Dormant private inventory extension

Legacy contract metadata remains 3.5.0. Ordinary resolution, exact bootstrap,
health, models, pinned-only references and deployment descriptors exclude every
scoped deployment. Scoped rows cannot be current. Explicit ResolveScoped and
DeploymentScoped match the complete audience and refuse expiry at its boundary.
Returned audience pointers do not alias the inventory.

Only a signed JSON body with scopedExecutionContractVersion: "3.6.0" enables
extended deployment descriptors. The response echoes that exact version. A
signed POST /internal/v1/models/query with only that version returns the observed
private catalogue for the normal Oxy importer. Model rows aggregate each pinned
reference once. Audience restrictions live only on the additional exact
deployment descriptors, joined by deployment ID, never on aggregated model rows. Legacy GET models remains byte
compatible. The version identifies a protocol, never catalogue evidence or a
price version. Oxy must still qualify actual rights, eligibility and prices;
none is supplied by the scope restriction itself.

The private publisher source permit defaults to nil. No flag, environment
variable, arbitrary input or generic Jev classification enables it. Its source
reviewed candidate requires authenticated canonical dated identity discovery,
the exact discovery and execution key, current normal publication evidence,
normal withholding, actual loaded provider card/source versions and exact
published token prices. Missing evidence fails closed for the private candidate,
including report-only withholding. This does not change ordinary withholding.

Legacy snapshot routing hashes and bodies are unchanged without a private row.
Private hash content additionally binds the whole audience (including policy,
principal and immutable card identities) and actual observed catalogue evidence.
Private rows do not affect observed-date history for ordinary model lines.

This source change activates no route, permit, credential, migration or inference.

## Same-cycle private publication dependencies

The canonical publisher passes its real immutable rate-card file to the private
factory and the Decider produced by that cycle's actual PostgreSQL evidence
read. The factory requires the Decider's timestamp to equal the snapshot time;
it derives the published quote inside `providercost` and binds the card's
version and source version. Authenticated discovery must independently report
that exact quote, dated model identity and execution key.

The ordinary evidence-read failure path still carries forward unexpired
withholding decisions. It returns no Decider, so that fallback cannot authorize
a private row. The public snapshot builder carries no private permit. The
source getter remains nil, so the production entrypoint does not load an
optional card or activate a deployment. Activating a reviewed source permit is
a separate change shared with Oxy, with an actual card version and finite
expiry.

A failed private prerequisite or unverified private discovery omits the private
row from the newly built snapshot and emits a fixed diagnostic. Ordinary routes
continue refreshing; a previous private row is never carried forward by that
fallback. A snapshot with no ordinary or verified private route still fails the
existing empty-inventory guard.
