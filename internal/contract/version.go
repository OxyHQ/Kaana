// Package contract holds Kaana's Go representation of the Oxy↔data-plane
// inference contract, `@kaana.ai/contracts` (`packages/contracts` in this
// repository, Kaana ADR 0002).
//
// The types here are not a convenience mirror. They are the wire, and the
// authority for what the wire says is the Zod source, never this package:
// `descriptor.json` is generated from it by `tools/contract` and
// `descriptor_test.go` compares every type below against that descriptor field
// by field. A field renamed, added, removed or made optional on either side is a
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
// Asserted against the contract's INFERENCE_CONTRACT_VERSION by
// descriptor_test.go, so changing the contract set without revisiting this
// constant fails the build. 4.0.0 is the set first published as
// `@kaana.ai/contracts`; 4.1.0 adds input transcription inside a realtime
// session (its usage unit and catalogue flag); 4.2.0 adds what
// https://kaana.ai/v1 speaks beyond it (the JSON transcription upload and the
// public response headers and realtime subprotocols); 4.3.0 adds the
// declared catalogue (identity, licence, data policy and list prices) that
// GET /internal/v1/models carries to Oxy; 4.4.0 adds the per-deployment
// `commercial` member of the deployment descriptors and the grant step's
// `sameModelFailover`; 4.5.0 adds the envelope's `transcription` member (a
// file transcription's language); 5.0.0 publishes what each voice model
// accepts (capabilities.realtime.parameters, audio.detectsLanguage and
// audio.transcriptionPrompt) and removes what no server served (the realtime
// temperature and tool strict mode, limits.maxTextItems, transcription
// timestamps, segments and words); 6.0.0 is one canonical API (Kaana ADR
// 0003): the catalogue loses what 5.0.0 published of each provider's wire
// (capabilities.realtime.parameters, audio.detectsLanguage and
// audio.transcriptionPrompt) and gains each voice model's pricePerMinute;
// 7.0.0 serves decisions as an ordinary grant (the request's model is
// optional) and removes the one-off source approvals: the grant's exactRoute,
// the canonical input hash, the scoped and Private Auto envelopes v3 and v4,
// and a deployment's scopedExecution.
// Oxy reads it from the health handshake but compares no version against it.
// 8.0.0 separates signed first-party authority from external scoped authority;
// 8.1.0 adds image input (Kaana ADR 0006): the image rules, the declared
// model's `imageInput` and the grant fact `inputImages`.
const ContractVersion = "8.1.0"

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
