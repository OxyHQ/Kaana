// Package customerlimit isolates transient throttle and refusal state to one
// exact customer credential generation. It is separate from deployment
// rotation so customer traffic can never open or close Kaana's platform lane.
package customerlimit

import (
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
)

const (
	defaultThrottleCooldown = 5 * time.Second
	defaultRejectedCooldown = 15 * time.Minute
	staleEntryAge           = time.Hour
)

// Reason describes why one exact generation is temporarily refused.
type Reason string

const (
	ReasonNone      Reason = ""
	ReasonThrottled Reason = "throttled"
	ReasonRejected  Reason = "rejected"
)

// Refusal is the safe retry information returned without resolving a secret.
type Refusal struct {
	Reason     Reason
	RetryAfter time.Duration
}

// Registry holds no secret material; its key is the exact signed selector.
type Registry struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[credentialstore.CustomerCredentialScope]*entry
}

type entry struct {
	reason        Reason
	probesAt      time.Time
	trialInFlight bool
	lastSeen      time.Time
}

// Permit represents one admitted attempt for a customer generation.
type Permit struct {
	registry *Registry
	scope    credentialstore.CustomerCredentialScope
	reported bool
}

// NewRegistry builds an isolated registry. The clock is injectable for tests.
func NewRegistry(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{now: now, entries: make(map[credentialstore.CustomerCredentialScope]*entry)}
}

// Admit allows a healthy generation, refuses it during a known cooldown, and
// admits exactly one half-open trial after that cooldown.
func (r *Registry) Admit(scope credentialstore.CustomerCredentialScope) (*Permit, Refusal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.removeStaleLocked(now)
	entry, known := r.entries[scope]
	if !known {
		return &Permit{registry: r, scope: scope}, Refusal{}
	}
	entry.lastSeen = now
	if now.Before(entry.probesAt) {
		return nil, Refusal{Reason: entry.reason, RetryAfter: entry.probesAt.Sub(now)}
	}
	if entry.trialInFlight {
		return nil, Refusal{Reason: entry.reason}
	}
	entry.trialInFlight = true
	return &Permit{registry: r, scope: scope}, Refusal{}
}

// Succeeded closes the exact generation lane.
func (p *Permit) Succeeded() { p.report(ReasonNone, 0, true) }

// Rejected opens the exact generation lane while Oxy records/disables it.
func (p *Permit) Rejected() { p.report(ReasonRejected, defaultRejectedCooldown, false) }

// Throttled honours the provider retry window for this generation only.
func (p *Permit) Throttled(retryAfter time.Duration) {
	if retryAfter <= 0 {
		retryAfter = defaultThrottleCooldown
	}
	p.report(ReasonThrottled, retryAfter, false)
}

// NotAttributable releases a half-open trial without changing the credential's
// prior state. Provider outages and request failures do not describe the key.
func (p *Permit) NotAttributable() { p.report(ReasonNone, 0, false) }

func (p *Permit) report(reason Reason, cooldown time.Duration, success bool) {
	if p == nil || p.reported {
		return
	}
	p.reported = true
	r := p.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	entry, known := r.entries[p.scope]
	if success {
		delete(r.entries, p.scope)
		return
	}
	if reason != ReasonNone {
		r.entries[p.scope] = entryFor(reason, now.Add(cooldown), now)
		return
	}
	if known {
		entry.trialInFlight = false
		entry.lastSeen = now
	}
}

// entryFor avoids accidentally carrying half-open state into a fresh
// throttle/rejection window.
func entryFor(reason Reason, probesAt, lastSeen time.Time) *entry {
	return &entry{reason: reason, probesAt: probesAt, lastSeen: lastSeen}
}

func (r *Registry) removeStaleLocked(now time.Time) {
	for scope, entry := range r.entries {
		if !entry.trialInFlight && !now.Before(entry.probesAt) && now.Sub(entry.lastSeen) > staleEntryAge {
			delete(r.entries, scope)
		}
	}
}
