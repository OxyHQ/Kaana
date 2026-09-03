// Package contract holds Kaana's Go representation of the Oxy↔data-plane
// inference contract published as `@oxyhq/contracts`.
//
// The types here are not a convenience mirror. They are the wire, and the
// authority for what the wire says is the published package, never this
// package: `descriptor.json` is generated from it by `tools/contract` and
// `contract_test.go` compares every type below against that descriptor field by
// field. A field renamed, added, removed or made optional on either side is a
// failing test, which is the only reason it is safe to write these structs by
// hand.
//
// Decoding rule, and it is deliberate: Kaana does NOT reject unknown fields on
// an inbound envelope. The contract states that adding an optional field is an
// additive change that does not bump a shape's version, so a strict decoder
// would turn every additive Oxy change into a production outage. What Kaana
// does reject is a `schemaVersion` it does not implement.
package contract

// ContractVersion is the version of the contract SET as a whole, exchanged in
// the health handshake so the two sides establish they were built against
// compatible definitions before a single request is served.
//
// Asserted against the published package's INFERENCE_CONTRACT_VERSION by
// contract_test.go, so bumping the pinned package without revisiting this
// constant fails the build.
const ContractVersion = "2.0.0"

// SchemaVersion is the per-shape version of the unchanged error and non-usage
// stream-event shapes. Each whole wire shape owns its version: the contract-set
// major can move without rewriting an unchanged message, and a shape that did
// change declares its own constant below.
const SchemaVersion = 1

// LegacyRequestEnvelopeVersion is the one transitional request shape this
// build accepts. It is restricted to a direct model target; the retired
// routing-profile slug arm is never interpreted by Kaana.
const LegacyRequestEnvelopeVersion = 1

// RequestEnvelopeVersion is the current Oxy→Kaana request-envelope version.
// Version 2 replaces a routing-profile slug with its exact opaque PostgreSQL
// identity. The versions are declared, never inferred from target fields.
const RequestEnvelopeVersion = 2

// SupportsRequestEnvelopeVersion reports the deliberately narrow transition
// window. Callers must still validate the version-specific target rules before
// interpreting the rest of the envelope.
func SupportsRequestEnvelopeVersion(version int) bool {
	return version == LegacyRequestEnvelopeVersion || version == RequestEnvelopeVersion
}

// UsageReportSchemaVersion is the normalized usage-report schema this build
// emits. Version 2 requires the exact deployment id.
const UsageReportSchemaVersion = 2

// StreamUsageEventSchemaVersion is the usage progress-event schema this build
// emits. Version 2 likewise names the exact deployment whose adapter measured
// the units.
const StreamUsageEventSchemaVersion = 2

// CredentialControlSchemaVersion is shared by the signed customer-credential
// mutation and outcome shapes. ProviderConnection's schemaVersion 2 belongs to
// Oxy metadata and does not change this Kaana wire boundary.
const CredentialControlSchemaVersion = 1
