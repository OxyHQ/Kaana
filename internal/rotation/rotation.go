// Package rotation decides which deployments are servable right now.
//
// It holds one circuit breaker and one health score per deployment. The unit is
// the DEPLOYMENT rather than the provider: a provider is usually several
// deployments in several regions, and taking all of them out because one region
// is failing throws away the capacity that failover exists to use.
//
// # What trips a breaker, and what must never trip one
//
// Only a failure attributable to the deployment counts — the upstream refusing,
// timing out, rate limiting, running out of quota, or rejecting Kaana's own
// credential. A request the provider could not express, a content filter, and a
// client that hung up say nothing about the deployment's health: they would
// produce exactly the same failure everywhere. Letting those count would let
// one customer's malformed traffic take a healthy route out of rotation for
// everybody, which is a denial of service with extra steps. That distinction is
// the load-bearing line in this package, and `Permit.NotAttributable` is how a
// caller states it rather than defaulting into it.
//
// # How a deployment gets back in
//
// A cooldown, then one real request. When the cooldown expires the breaker
// admits exactly ONE trial — not a burst, not everything that arrives — and the
// deployment's fate turns on it: a success moves it towards closed, a failure
// re-opens it with a doubled cooldown up to a ceiling. There is no synthetic
// probe, because a synthetic probe proves the provider is answering some other
// request than the one it is failing, and Kaana would be paying for it.
package rotation

import (
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// State is a breaker's coarse state.
type State string

const (
	// StateClosed is the normal state: requests are admitted.
	StateClosed State = "closed"
	// StateOpen means the deployment is out of rotation until its cooldown
	// expires.
	StateOpen State = "open"
	// StateHalfOpen means the cooldown has expired and the deployment is being
	// probed back in with one real request at a time.
	StateHalfOpen State = "half_open"
)

// Policy is how quickly a deployment leaves rotation and how it comes back.
type Policy struct {
	// FailuresToOpen is how many CONSECUTIVE attributable failures open a
	// closed breaker. Consecutive rather than a rate, because a rate needs a
	// window and a window needs a traffic assumption; a deployment that fails
	// three times in a row with nothing in between is failing.
	FailuresToOpen int
	// Cooldown is how long a breaker stays open the first time.
	Cooldown time.Duration
	// MaxCooldown caps the doubling, so a provider that has been down for an
	// hour is still retried within a bounded time rather than in a day.
	MaxCooldown time.Duration
	// SuccessesToClose is how many consecutive trial requests must succeed
	// before the breaker closes.
	SuccessesToClose int
	// ScoreWeight is the exponential moving average's weight on the newest
	// outcome. Higher reacts faster and is noisier.
	ScoreWeight float64
}

// DefaultPolicy is deliberately quick to open and quick to probe.
//
// The cost of opening a breaker wrongly is one route skipped for five seconds
// on a request that has another route to try; the cost of opening it too slowly
// is every request in that window paying the full upstream timeout first. Those
// are not symmetric, so the defaults lean towards opening.
func DefaultPolicy() Policy {
	return Policy{
		FailuresToOpen:   3,
		Cooldown:         5 * time.Second,
		MaxCooldown:      2 * time.Minute,
		SuccessesToClose: 1,
		ScoreWeight:      0.2,
	}
}

// Registry holds the breakers and scores for every deployment this process has
// seen.
type Registry struct {
	policy Policy
	now    func() time.Time

	mu       sync.Mutex
	breakers map[contract.DeploymentID]*breaker
}

// NewRegistry builds a registry. A zero-valued field in policy takes its
// default, so a caller can override one number without restating the rest.
func NewRegistry(policy Policy, now func() time.Time) *Registry {
	defaults := DefaultPolicy()
	if policy.FailuresToOpen <= 0 {
		policy.FailuresToOpen = defaults.FailuresToOpen
	}
	if policy.Cooldown <= 0 {
		policy.Cooldown = defaults.Cooldown
	}
	if policy.MaxCooldown < policy.Cooldown {
		policy.MaxCooldown = maxDuration(defaults.MaxCooldown, policy.Cooldown)
	}
	if policy.SuccessesToClose <= 0 {
		policy.SuccessesToClose = defaults.SuccessesToClose
	}
	if policy.ScoreWeight <= 0 || policy.ScoreWeight > 1 {
		policy.ScoreWeight = defaults.ScoreWeight
	}
	if now == nil {
		now = time.Now
	}
	return &Registry{policy: policy, now: now, breakers: make(map[contract.DeploymentID]*breaker)}
}

type breaker struct {
	state               State
	consecutiveFailures int
	trialSuccesses      int
	// trialInFlight is what makes half-open admit ONE request rather than
	// everything that arrives the moment the cooldown expires — which would be
	// a thundering herd onto the provider that just stopped failing.
	trialInFlight bool
	cooldown      time.Duration
	probesAt      time.Time
	// score is an exponential moving average of attributable outcomes, 1 for
	// served and 0 for failed. A deployment nobody has used yet scores 1: the
	// projection must distinguish "no evidence of failure" from known failure.
	score float64
}

// Permit is the right to attempt one request on one deployment.
//
// Exactly one of its three outcomes must be reported. A permit that is never
// reported holds a half-open trial slot forever, which is the one way this
// package can take a recovered deployment out of rotation permanently — so the
// caller reports it on every path, including the ones that return early.
type Permit struct {
	registry *Registry
	id       contract.DeploymentID
	trial    bool
	reported bool
}

// Trial reports whether this permit is a half-open probe, i.e. whether the
// deployment's return to rotation turns on it.
func (p *Permit) Trial() bool { return p.trial }

// Succeeded records that the deployment served the request.
func (p *Permit) Succeeded() { p.report(outcomeServed) }

// Failed records a failure attributable to the deployment.
func (p *Permit) Failed() { p.report(outcomeFailed) }

// NotAttributable records that the request failed for a reason that says
// nothing about this deployment — a request the provider cannot express, a
// content filter, a client that hung up. It moves neither the breaker nor the
// score, and releases a trial slot so the deployment can still be probed.
func (p *Permit) NotAttributable() { p.report(outcomeIrrelevant) }

type outcome int

const (
	outcomeServed outcome = iota
	outcomeFailed
	outcomeIrrelevant
)

func (p *Permit) report(result outcome) {
	if p == nil || p.reported {
		// Idempotent on purpose: a caller that reports on both the happy path
		// and in a deferred cleanup must not double-count a failure into
		// opening a breaker.
		return
	}
	p.reported = true
	p.registry.record(p.id, p.trial, result)
}

// Admit reports whether a deployment may serve a request now.
//
// A refusal is not an error and not a failure: it is the breaker doing its job,
// and the caller's answer is to try the next candidate.
func (r *Registry) Admit(id contract.DeploymentID) (*Permit, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry := r.breakerLocked(id)
	now := r.now()

	switch entry.state {
	case StateClosed:
		return &Permit{registry: r, id: id}, true

	case StateOpen:
		if now.Before(entry.probesAt) {
			return nil, false
		}
		entry.state = StateHalfOpen
		entry.trialSuccesses = 0
		entry.trialInFlight = true
		return &Permit{registry: r, id: id, trial: true}, true

	case StateHalfOpen:
		if entry.trialInFlight {
			return nil, false
		}
		entry.trialInFlight = true
		return &Permit{registry: r, id: id, trial: true}, true
	}
	return nil, false
}

func (r *Registry) record(id contract.DeploymentID, trial bool, result outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry := r.breakerLocked(id)
	if trial {
		entry.trialInFlight = false
	}

	switch result {
	case outcomeIrrelevant:
		// Deliberately nothing. The score and the failure count both describe
		// the deployment, and this request described the request.
		return

	case outcomeServed:
		entry.score = r.blend(entry.score, 1)
		entry.consecutiveFailures = 0
		if !trial {
			return
		}
		entry.trialSuccesses++
		if entry.trialSuccesses >= r.policy.SuccessesToClose {
			entry.state = StateClosed
			entry.trialSuccesses = 0
			// The cooldown resets only on a full return to rotation, so a
			// deployment that flaps in and out keeps its escalated backoff.
			entry.cooldown = 0
		}

	case outcomeFailed:
		entry.score = r.blend(entry.score, 0)
		entry.consecutiveFailures++
		switch {
		case trial:
			// The probe failed: straight back to open with a longer cooldown.
			entry.cooldown = r.escalate(entry.cooldown)
			entry.state = StateOpen
			entry.probesAt = r.now().Add(entry.cooldown)
			entry.trialSuccesses = 0
		case entry.consecutiveFailures >= r.policy.FailuresToOpen:
			entry.cooldown = r.escalate(entry.cooldown)
			entry.state = StateOpen
			entry.probesAt = r.now().Add(entry.cooldown)
		}
	}
}

func (r *Registry) blend(current, observed float64) float64 {
	return current*(1-r.policy.ScoreWeight) + observed*r.policy.ScoreWeight
}

func (r *Registry) escalate(current time.Duration) time.Duration {
	if current <= 0 {
		return r.policy.Cooldown
	}
	doubled := current * 2
	if doubled > r.policy.MaxCooldown {
		return r.policy.MaxCooldown
	}
	return doubled
}

func (r *Registry) breakerLocked(id contract.DeploymentID) *breaker {
	entry, known := r.breakers[id]
	if !known {
		entry = &breaker{state: StateClosed, score: 1}
		r.breakers[id] = entry
	}
	return entry
}

// SoonestProbe reports how long until the earliest of these deployments will
// admit a request again, and whether any of them will.
//
// The caller puts it on the retry hint of the refusal it returns, so a client
// backing off is told the truth instead of guessing at it.
func (r *Registry) SoonestProbe(ids []contract.DeploymentID, at time.Time) (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	soonest := time.Duration(0)
	found := false
	for _, id := range ids {
		entry, known := r.breakers[id]
		if !known {
			continue
		}
		wait := entry.probesAt.Sub(at)
		if wait < 0 {
			wait = 0
		}
		if !found || wait < soonest {
			soonest, found = wait, true
		}
	}
	return soonest, found
}

/* -------------------------------------------------------------------------- */
/*  Projection                                                                */
/* -------------------------------------------------------------------------- */

// Health is the operator-facing projection of one deployment's rotation state.
// It carries no upstream URL, no credential and no error text from a provider.
type Health struct {
	DeploymentID        contract.DeploymentID `json:"deploymentId"`
	State               State                 `json:"state"`
	Score               float64               `json:"score"`
	ConsecutiveFailures int                   `json:"consecutiveFailures"`
	// ProbesAt is when an open breaker will admit its next trial. Absent while
	// the breaker is closed, because there is nothing to wait for.
	ProbesAt *contract.Timestamp `json:"probesAt,omitempty"`
}

// Project reports the current state of the named deployments, in the order
// given. Deployments this process has never routed to are reported in their
// initial state rather than omitted, so the projection is the inventory's shape
// and not a traffic history.
func (r *Registry) Project(ids []contract.DeploymentID) []Health {
	r.mu.Lock()
	defer r.mu.Unlock()

	projected := make([]Health, 0, len(ids))
	for _, id := range ids {
		entry := r.breakerLocked(id)
		health := Health{
			DeploymentID:        id,
			State:               entry.state,
			Score:               roundScore(entry.score),
			ConsecutiveFailures: entry.consecutiveFailures,
		}
		if entry.state != StateClosed {
			probesAt := contract.NewTimestamp(entry.probesAt)
			health.ProbesAt = &probesAt
		}
		projected = append(projected, health)
	}
	return projected
}

// Retain drops breakers for deployments that are no longer in the inventory, so
// a long-running process that has seen many snapshots does not accumulate the
// history of every deployment that ever existed.
func (r *Registry) Retain(known []contract.DeploymentID) {
	keep := make(map[contract.DeploymentID]struct{}, len(known))
	for _, id := range known {
		keep[id] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.breakers {
		if _, wanted := keep[id]; !wanted {
			delete(r.breakers, id)
		}
	}
}

func roundScore(score float64) float64 {
	return float64(int(score*1000+0.5)) / 1000
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
