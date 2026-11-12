package rotation

import (
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// FirstOutputWindow is how long a time-to-first-output observation is
// evidence. Older than this and the deployment reads as unobserved: a
// deployment that was slow ten minutes ago and has had no traffic since is not
// known to be slow now, and a stale reading would keep it behind its peers
// forever (ADR 0008, decision 3).
const FirstOutputWindow = 5 * time.Minute

// firstOutputWeight is the exponential moving average's weight on the newest
// observation.
const firstOutputWeight = 0.3

type firstOutput struct {
	average time.Duration
	at      time.Time
}

// ObserveFirstOutput records how long a deployment this permit admitted took to
// produce its first output. It is the only latency evidence Kaana orders by,
// and it only ever breaks ties (publicapi.resolveGrant): it never excludes a
// deployment and never touches the breaker or the health score.
//
// Report it for a request the deployment served. A non-positive duration says
// nothing and is ignored, and a nil permit (a customer-credential route, which
// never touches platform rotation) records nothing.
func (p *Permit) ObserveFirstOutput(elapsed time.Duration) {
	if p == nil || elapsed <= 0 {
		return
	}
	p.registry.observeFirstOutput(p.id, elapsed)
}

func (r *Registry) observeFirstOutput(id contract.DeploymentID, elapsed time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.firstOutputs == nil {
		r.firstOutputs = make(map[contract.DeploymentID]*firstOutput)
	}
	now := r.now()
	entry := r.firstOutputs[id]
	if entry == nil || now.Sub(entry.at) > FirstOutputWindow {
		r.firstOutputs[id] = &firstOutput{average: elapsed, at: now}
		return
	}
	entry.average = time.Duration(firstOutputWeight*float64(elapsed) + (1-firstOutputWeight)*float64(entry.average))
	entry.at = now
}

// FirstOutput is the deployment's recent typical time to first output. ok is
// false when nothing was observed within FirstOutputWindow: unknown is not
// slow, and a caller must not rank it as if it were.
func (r *Registry) FirstOutput(id contract.DeploymentID) (time.Duration, bool) {
	if r == nil {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.firstOutputs[id]
	if entry == nil || r.now().Sub(entry.at) > FirstOutputWindow {
		return 0, false
	}
	return entry.average, true
}

// ObserveThroughput records the output tokens per second a deployment sustained
// after its first output. Like ObserveFirstOutput it only orders and demotes
// routes (`provider.sort`, `preferred_min_throughput`): it never excludes one
// and never touches the breaker. A non-positive reading is ignored.
func (p *Permit) ObserveThroughput(tokensPerSecond float64) {
	if p == nil || tokensPerSecond <= 0 {
		return
	}
	p.registry.observeThroughput(p.id, tokensPerSecond)
}

type throughput struct {
	average float64
	at      time.Time
}

func (r *Registry) observeThroughput(id contract.DeploymentID, tokensPerSecond float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.throughputs == nil {
		r.throughputs = make(map[contract.DeploymentID]*throughput)
	}
	now := r.now()
	entry := r.throughputs[id]
	if entry == nil || now.Sub(entry.at) > FirstOutputWindow {
		r.throughputs[id] = &throughput{average: tokensPerSecond, at: now}
		return
	}
	entry.average = firstOutputWeight*tokensPerSecond + (1-firstOutputWeight)*entry.average
	entry.at = now
}

// Throughput is the deployment's recent typical output tokens per second. ok is
// false when nothing was observed within FirstOutputWindow.
func (r *Registry) Throughput(id contract.DeploymentID) (float64, bool) {
	if r == nil {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.throughputs[id]
	if entry == nil || r.now().Sub(entry.at) > FirstOutputWindow {
		return 0, false
	}
	return entry.average, true
}
