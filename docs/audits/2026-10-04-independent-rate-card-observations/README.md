# Independent rate-card observations (inert source)

The loader accepts disjoint documents and retains each original version, source,
observation, effective time and expiry. Measurement and private publication resolve
that evidence by deployment; startup registers the original documents separately.
A duplicate version or deployment fails closed. Single-file behavior is retained.

No production card, source-reviewed getter, task configuration or schema changes.
`configs/provider-rates.json` remains byte identical, SHA256
`5a094d5d27005f7182c99cb19129d328f48576147940db91bbb5fc0008219968`.
Current entrypoints still load that one existing document. A future source-reviewed
activation must explicitly load/package the separate Jev document in both serving
and publishing processes and keep the old xAI document unchanged.

Validation: final `make check` with pinned toolchain; exact deployment attribution
mutant fails and restored source passes; own PostgreSQL17 runs canonical migrations
and registers/replays the two documents, including different expiries, without
re-dating either. The first PostgreSQL setup lacked the canonical CI provisioning
roles and failed before the new assertion; that setup failure is retained. The
fixture then provisions only the five existing CI roles on its own random server.
No production connection or authenticated upstream inference is used.

Base63c1b80 is the accepted PR152 source series. Main44e8 was merged while this
worktree was running and could not be fetched through the local Git transport;
this proof does not claim whole-tree equality to that merge. Rebase/composition
must preserve current main and recheck relevant source bytes before release.
