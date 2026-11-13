// Package operatingmetrics turns Kaana's in-memory operating state into EMF
// gauges (internal/emf): provider key pools, the inventory snapshot's age, and
// what each deployment recently did.
//
// It runs on the publisher's flush tick (emf.Options.Sample), reads state that
// already exists under the locks that state already has, and records nothing on
// a request path. It never calls a provider: an adapter's Health makes a network
// probe, so key counts come from the key pool's own projection instead.
//
// # What may be a dimension
//
// Provider, a small closed set, is the only dimension. A deployment id is
// written as the plain property DeploymentId of the line: queryable in
// CloudWatch Logs Insights, and never billed as a metric. Nothing a caller
// typed, no key id, model name, request id or account, reaches a line: the
// inputs here are counts, ratios and durations plus the two identifiers above,
// which are opaque ids Oxy already receives on every usage report.
//
// # What is omitted
//
// A deployment that is idle (breaker closed, no failures, no attempt in the
// last thirty minutes) is skipped: its figures would all be defaults, and there
// are hundreds of such deployments. A measurement the window did not observe
// (uptime, time to first output, throughput) is omitted rather than recorded as
// zero, because zero would read as a perfect or a stalled deployment.
package operatingmetrics

import (
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/deploymentstats"
	"github.com/OxyHQ/Kaana/internal/emf"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/rotation"
)

// Providers lists the registered adapters; *provider.Registry satisfies it.
type Providers interface {
	All() []provider.Registrant
}

// Sources is the state the sampler reads.
type Sources struct {
	Inventory *inventory.Store
	Rotation  *rotation.Registry
	Stats     *deploymentstats.Registry
	Providers Providers
	// Now is the clock for key retirement; nil takes time.Now.
	Now func() time.Time
}

var windows = []struct {
	span    time.Duration
	metrics emf.DeploymentWindowMetrics
}{
	{5 * time.Minute, emf.Deployment5m},
	{30 * time.Minute, emf.Deployment30m},
}

// Sample records one reading of every gauge. It is the emf.Options.Sample hook.
func (s Sources) Sample(out emf.Recorder) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	s.sampleProviders(out, now())
	s.sampleInventory(out)
}

func (s Sources) sampleProviders(out emf.Recorder, at time.Time) {
	if s.Providers == nil {
		return
	}
	for _, registrant := range s.Providers.All() {
		source, ok := registrant.(provider.PlatformCredentialSource)
		if !ok {
			continue
		}
		pool := source.PlatformCredentials()
		if pool == nil {
			continue
		}
		health := pool.Projection(at)
		labels := emf.Labels{"Provider": string(registrant.Provider())}
		out.Record(emf.ProviderKeysDeclared, float64(health.Declared), labels)
		out.Record(emf.ProviderKeysUsable, float64(health.Usable), labels)
		out.Record(emf.ProviderKeysOut, float64(health.Declared-health.Usable), labels)
	}
}

func (s Sources) sampleInventory(out emf.Recorder) {
	status := s.Inventory.Status()
	endpoints := s.Inventory.Current().Deployments()
	out.Record(emf.InventorySnapshotAge, float64(status.AgeSeconds), nil)
	out.Record(emf.InventoryStale, boolGauge(!status.ServesUnpinnedReferences), nil)
	out.Record(emf.InventoryDeployments, float64(len(endpoints)), nil)

	ids := make([]contract.DeploymentID, len(endpoints))
	for index, endpoint := range endpoints {
		ids[index] = endpoint.DeploymentID
	}
	recent := s.Stats.Project(ids)
	for index, health := range s.Rotation.Project(ids) {
		var observed []deploymentstats.Window
		if index < len(recent) {
			observed = recent[index].Windows
		}
		if idle(health, observed) {
			continue
		}
		labels := emf.Labels{"Provider": string(endpoints[index].Provider), "DeploymentId": string(health.DeploymentID)}
		if state, known := breakerState(health.State); known {
			out.Record(emf.DeploymentBreakerState, state, labels)
		}
		out.Record(emf.DeploymentHealthScore, health.Score, labels)
		out.Record(emf.DeploymentConsecutiveFailures, float64(health.ConsecutiveFailures), labels)
		for _, want := range windows {
			for _, window := range observed {
				if time.Duration(window.Seconds)*time.Second == want.span {
					recordWindow(out, want.metrics, window, labels)
				}
			}
		}
	}
}

func recordWindow(out emf.Recorder, metrics emf.DeploymentWindowMetrics, window deploymentstats.Window, labels emf.Labels) {
	out.Record(metrics.Attempts, float64(window.Attempts), labels)
	out.Record(metrics.Succeeded, float64(window.Succeeded), labels)
	out.Record(metrics.Throttled, float64(window.Throttled), labels)
	out.Record(metrics.ToolCallAttempts, float64(window.ToolCallAttempts), labels)
	out.Record(metrics.ToolCallErrors, float64(window.ToolCallErrors), labels)
	if window.Uptime != nil {
		out.Record(metrics.Uptime, *window.Uptime, labels)
	}
	if p := window.TimeToFirstOutputMs; p != nil {
		out.Record(metrics.TimeToFirstOutputP50, p.P50, labels)
		out.Record(metrics.TimeToFirstOutputP99, p.P99, labels)
	}
	if p := window.TokensPerSecond; p != nil {
		out.Record(metrics.TokensPerSecondP50, p.P50, labels)
	}
}

// idle reports a deployment with nothing to say: in rotation, never failing and
// not attempted within the longest published window.
func idle(health rotation.Health, observed []deploymentstats.Window) bool {
	if health.State != rotation.StateClosed || health.ConsecutiveFailures != 0 {
		return false
	}
	longest := windows[len(windows)-1].span
	for _, window := range observed {
		if time.Duration(window.Seconds)*time.Second <= longest && window.Attempts > 0 {
			return false
		}
	}
	return true
}

// breakerState is the gauge for a breaker state: 0 closed, 1 open, 2 half open.
func breakerState(state rotation.State) (float64, bool) {
	switch state {
	case rotation.StateClosed:
		return 0, true
	case rotation.StateOpen:
		return 1, true
	case rotation.StateHalfOpen:
		return 2, true
	}
	return 0, false
}

func boolGauge(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
