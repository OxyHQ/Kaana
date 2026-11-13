package emf

// Kaana's metric definitions. Every dimension is a CLOSED vocabulary: Operation
// is the grant operation, Outcome a public error code or "completed", Provider a
// provider slug. Free-form values (a request id, a model, a credential) are never
// labels.
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

// Operating state, sampled once per flush interval from memory
// (internal/operatingmetrics).
//
// These are gauges. A key pool and a deployment are read, not observed, so there
// is no per-request recording and nothing on the hot path.
//
// Cost control. A deployment id is not a closed vocabulary (the inventory holds
// hundreds), so it is NEVER a dimension: it is the plain property DeploymentId
// of the line, free to store and groupable in Logs Insights. Provider is the one
// dimension, a small closed set. Billed series are therefore
// metric names x (1 + providers), and no deployment can add one.
var (
	// ProviderKeysDeclared, ProviderKeysUsable and ProviderKeysOut are one
	// provider's key pool by count: declared, usable now, and retired from
	// rotation. A pool draining towards zero is otherwise invisible until the
	// request that finds it empty. Counts only; never a key id.
	ProviderKeysDeclared = Metric{Name: "ProviderKeysDeclared", Unit: "Count", Kind: Gauge, DimensionSets: [][]string{{"Provider"}}}
	ProviderKeysUsable   = Metric{Name: "ProviderKeysUsable", Unit: "Count", Kind: Gauge, DimensionSets: [][]string{{"Provider"}}}
	ProviderKeysOut      = Metric{Name: "ProviderKeysOut", Unit: "Count", Kind: Gauge, DimensionSets: [][]string{{"Provider"}}}

	// InventorySnapshotAge is the age of the served inventory snapshot. A value
	// that only grows means the publisher stopped.
	InventorySnapshotAge = Metric{Name: "InventorySnapshotAgeSeconds", Unit: "Seconds", Kind: Gauge}
	// InventoryStale is 1 once the snapshot is past its staleness horizon and
	// unpinned references are refused, 0 while it is inside it.
	InventoryStale = Metric{Name: "InventoryStale", Unit: "None", Kind: Gauge}
	// InventoryDeployments is how many deployments the snapshot holds.
	InventoryDeployments = Metric{Name: "InventoryDeployments", Unit: "Count", Kind: Gauge}

	// DeploymentBreakerState is 0 closed, 1 open, 2 half open.
	DeploymentBreakerState = Metric{Name: "DeploymentBreakerState", Unit: "None", Kind: Gauge, DimensionSets: [][]string{{"Provider"}}}
	// DeploymentHealthScore is the rotation health score, 0 to 1.
	DeploymentHealthScore = Metric{Name: "DeploymentHealthScore", Unit: "None", Kind: Gauge, DimensionSets: [][]string{{"Provider"}}}
	// DeploymentConsecutiveFailures is the run of attributable failures that
	// opens a breaker.
	DeploymentConsecutiveFailures = Metric{Name: "DeploymentConsecutiveFailures", Unit: "Count", Kind: Gauge, DimensionSets: [][]string{{"Provider"}}}

	// Deployment5m and Deployment30m are what a deployment did over the last
	// five and thirty minutes (internal/deploymentstats). The window is part of
	// the metric NAME, not a dimension: the always-present [Service] aggregate of
	// a Window dimension would add 5-minute and 30-minute figures together. The
	// one-day window is not published, to keep the metric count low; it stays on
	// the health route.
	Deployment5m  = deploymentWindow("5m")
	Deployment30m = deploymentWindow("30m")
)

// DeploymentWindowMetrics are one window's metrics for a deployment. Counts are
// gauges because the window slides. Uptime, time to first output and throughput
// are omitted, not zero, when the window did not observe them.
type DeploymentWindowMetrics struct {
	Attempts, Succeeded, Throttled             Metric
	ToolCallAttempts, ToolCallErrors           Metric
	Uptime                                     Metric
	TimeToFirstOutputP50, TimeToFirstOutputP99 Metric
	TokensPerSecondP50                         Metric
}

func deploymentWindow(window string) DeploymentWindowMetrics {
	gauge := func(name, unit string) Metric {
		return Metric{Name: name + window, Unit: unit, Kind: Gauge, DimensionSets: [][]string{{"Provider"}}}
	}
	return DeploymentWindowMetrics{
		Attempts:             gauge("DeploymentAttempts", "Count"),
		Succeeded:            gauge("DeploymentSucceeded", "Count"),
		Throttled:            gauge("DeploymentThrottled", "Count"),
		ToolCallAttempts:     gauge("DeploymentToolCallAttempts", "Count"),
		ToolCallErrors:       gauge("DeploymentToolCallErrors", "Count"),
		Uptime:               gauge("DeploymentUptime", "None"),
		TimeToFirstOutputP50: gauge("DeploymentTimeToFirstOutputP50Ms", "Milliseconds"),
		TimeToFirstOutputP99: gauge("DeploymentTimeToFirstOutputP99Ms", "Milliseconds"),
		TokensPerSecondP50:   gauge("DeploymentTokensPerSecondP50", "Count/Second"),
	}
}
