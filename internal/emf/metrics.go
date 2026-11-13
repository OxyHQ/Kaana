package emf

// Kaana's metric definitions. Every dimension is a CLOSED vocabulary: Operation
// is the grant operation, Outcome a public error code or "completed". Free-form
// values (a request id, a model, a provider, a credential) are never labels.
var (
	// PublicRequest is one request on the public lane: SampleCount is the request
	// count, percentiles the end-to-end latency.
	PublicRequest = Metric{Name: "PublicRequestLatency", Unit: "Milliseconds", Kind: Distribution, DimensionSets: [][]string{{"Operation"}, {"Outcome"}}}
	// GrantRoundTrip is the authorization call to Oxy.
	GrantRoundTrip = Metric{Name: "GrantLatency", Unit: "Milliseconds", Kind: Distribution}
	// UpstreamExchange is the time spent waiting on providers within a request.
	UpstreamExchange = Metric{Name: "UpstreamLatency", Unit: "Milliseconds", Kind: Distribution}
	// RateLimited is a request the per-credential limiter refused.
	RateLimited = Metric{Name: "RateLimited", Unit: "Count", Kind: Sum}
	// SettlementWritten and SettlementWriteFailed count outbox writes; a failure
	// loses the settlement, so it is the metric to alarm on.
	SettlementWritten     = Metric{Name: "SettlementWritten", Unit: "Count", Kind: Sum}
	SettlementWriteFailed = Metric{Name: "SettlementWriteFailed", Unit: "Count", Kind: Sum}
	// SettlementDelivered and SettlementDeliveryFailed count deliveries to Oxy.
	SettlementDelivered      = Metric{Name: "SettlementDelivered", Unit: "Count", Kind: Sum}
	SettlementDeliveryFailed = Metric{Name: "SettlementDeliveryFailed", Unit: "Count", Kind: Sum}
)
