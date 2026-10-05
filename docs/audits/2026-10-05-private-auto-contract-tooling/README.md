# Private Auto candidate contract gate

This tooling follow-up consumes the exact unpublished contracts candidate supplied by Coverage, not the published package with the same provisional 4.9.1 label. Root chooses the final release version. No package pin, lockfile, published descriptor or production source getter changes here.

The canonical `tools/contract/generate.mjs` is copied byte-identically into an isolated tree whose `node_modules/@oxy.so/contracts` symlink resolves to the extracted candidate outside that directory. `OXY_CONTRACTS_LOCAL_SOURCE=OxyHQ/oxy@008ad9c1c6855b672f314ec9f7c681376e41dc40` makes the generated descriptor explicitly `unpublished-local-build`. Output is retained under this audit as a provisional descriptor, never installed over `internal/contract/descriptor.json`. The ordinary global contract constant remains 3.5.0; the separately negotiated private contract constant is 3.7.0 / envelope 4.

`TestPrivateAutoCandidateDescriptor` uses the existing Go structural comparator for the four private named shapes. Private input uses only its two strict private fields rather than advertising the other legacy input variants. Source/principal/child structs and their nested references are actual production Go types. The separate value gate parses six Go-produced source/principal/child/input/envelope fixtures with the actual candidate Zod schemas and requires eight rejection controls. These include delegation, foreign application, altered permanent operation and caller input in source authority.

`validate-private-auto.mjs` invokes the actual signed Go HTTP metadata handler fixture too. It validates that the independently negotiated 3.7 descriptor carries the exact source approval and identity fields. Its shared approval leaf uses the shared Zod schema; the enclosing negotiated response remains a Kaana/Oxy handshake shape, not a newly claimed shared schema. The fixture never calls a provider or advertises an active approval.

The previous candidate passed the old controls but accepted delegated `attribution.userId`; the added must-reject control retained a RED. Coverage corrected that schema guard, ran its contract tests and built+packed in the same command. The new candidate passes unchanged goldens plus the new control. No source authority or source hash changed.

To reproduce after extracting the reviewed archive and linking its exact Zod dependency:

```bash
OXY_CONTRACTS_LOCAL_SOURCE=OxyHQ/oxy@008ad9c1c6855b672f314ec9f7c681376e41dc40 OXY_CONTRACTS_CANDIDATE_PACKAGE=/absolute/extracted/package bun tools/contract/validate-private-auto.mjs
OXY_CONTRACTS_LOCAL_SOURCE=OxyHQ/oxy@008ad9c1c6855b672f314ec9f7c681376e41dc40 KAANA_PRIVATE_AUTO_CANDIDATE_DESCRIPTOR=/absolute/provisional/descriptor.json go test ./internal/contract -run '^TestPrivateAutoCandidateDescriptor$' -count=1
```

The explicit candidate command refuses a missing source label or missing private schemas. The standard published validator remains unchanged (80 shapes / 29 controls). Coordinated final contract publication, exact pins/lock and full descriptor mapping/regeneration remain a later reviewed composition. This audit does not assert full published-descriptor compatibility or production Auto acceptance. No AWS, provider, publication or activation occurred.
