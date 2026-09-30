package publisher

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// # Withheld from publication
//
// A provider answering `GET /models` proves the credential authenticates, not
// that it can be served: Cerebras, OpenAI and CheaperInference all list models
// for an account that answers every completion with a 402. Publishing those
// deployments hands Oxy routes that fail every request. So before a discovered
// deployment is written, the publisher asks what KAANA has already been told
// about serving it, and withholds the deployment while that evidence says it
// cannot be served now.
//
// Every input is a report Kaana already persisted from real traffic or from an
// operator; nothing here sends a request of its own:
//
//   - the exact key the deployment executes on is retired
//     (`provider_credential_runtime_state`, written by a provider's own
//     exhaustion or refusal), until the provider's or Kaana's return time;
//   - that key carries fresh operator-recorded capacity evidence reading zero
//     (migration 0017), until the evidence stops being fresh;
//   - the deployment's own attempts on that key, since its last success, are a
//     streak of provider-side failures (migration 0019) — a single credential
//     refusal, or `Failures` deployment faults spanning at least `FailureSpan`.
//
// Withholding is PUBLICATION, which is Kaana's authority. It never reorders
// anything: order is Oxy's, and inventory order is presentation only.
//
// # Hysteresis, and why the snapshot does not flap every cycle
//
// A withheld deployment receives no traffic, so no success can ever arrive to
// restore it. It therefore comes back on a clock: the quarantine after its
// last counted failure is as long as the streak has lasted, clamped to
// [MinQuarantine, MaxQuarantine]. A deployment that failed for twenty minutes
// is withheld thirty; one dead for a day is re-published for a trial every six
// hours. The trial is real traffic through the serving breaker (one request at
// a time once it opens), and Oxy's authorized failover absorbs a failed one
// before any output. A success ends the streak, and the next cycle publishes.
//
// Withholding changes the set of routes, so it changes `snapshotId` — it must,
// because routing changed. What does NOT enter the hash is the evidence: the
// reason, the failure count and the return time are carried in the snapshot's
// separate `withheld` list, so re-stating an unchanged decision every cycle
// leaves `snapshotId` exactly where it was.

// WithholdReason says which piece of evidence withheld a deployment.
type WithholdReason string

const (
	// WithheldKeyRetired: the exact key is retired by the provider's own
	// exhaustion or refusal, and serving would refuse the deployment anyway.
	WithheldKeyRetired WithholdReason = "key_retired"
	// WithheldNoUsableKey: the deployment resolves to no single key (serving
	// refuses it as unbound) and every key its provider holds is retired.
	WithheldNoUsableKey WithholdReason = "no_usable_key"
	// WithheldCapacityExhausted: fresh operator-recorded capacity evidence says
	// the exact key has nothing left.
	WithheldCapacityExhausted WithholdReason = "capacity_exhausted"
	// WithheldCredentialRefused: since its last success, the deployment's key
	// was refused by the provider (billing or credential) and the quarantine
	// after that refusal has not elapsed.
	WithheldCredentialRefused WithholdReason = "credential_refused"
	// WithheldDeploymentFailing: since its last success, the deployment has
	// failed for provider-side reasons often enough and for long enough.
	WithheldDeploymentFailing WithholdReason = "deployment_failing"
)

// Withholding is one decision to leave a discovered deployment out.
type Withholding struct {
	Reason WithholdReason
	// KeyID is the exact opaque key the evidence is about. Logged; never
	// written into the snapshot, which carries no credential state.
	KeyID string
	// Until is when this evidence stops withholding the deployment on its own:
	// the key's return, the evidence's freshness, or the quarantine's end.
	Until time.Time
	// Failures is how many counted failures the streak holds; zero for a
	// decision that is not a streak.
	Failures int
}

// Withhold decides one discovered deployment. False publishes it.
type Withhold func(contract.DeploymentID, contract.ProviderSlug) (Withholding, bool)

// Defaults for WithholdPolicy. Conservative on purpose: withholding a working
// deployment is an outage Kaana caused, so it takes a sustained streak with no
// success at all, and transient classes never count.
const (
	DefaultWithholdFailures      = 5
	DefaultWithholdFailureSpan   = 10 * time.Minute
	DefaultWithholdMinQuarantine = 30 * time.Minute
	DefaultWithholdMaxQuarantine = 6 * time.Hour
	DefaultWithholdLookback      = 24 * time.Hour
	// MaxWithholdLookback bounds the streak read; the database function
	// refuses anything older than eight days.
	MaxWithholdLookback = 7 * 24 * time.Hour
)

// WithholdPolicy is the operator-settable half of the rule.
type WithholdPolicy struct {
	// Failures is how many deployment faults since the last success withhold
	// a deployment. A credential refusal needs one: it is the provider's own
	// report about the exact key.
	Failures int
	// FailureSpan is how long those faults must span, first to last, so a burst
	// inside one bad minute never withholds.
	FailureSpan time.Duration
	// MinQuarantine and MaxQuarantine clamp how long a streak withholds after
	// its last failure. The quarantine is the streak's own length.
	MinQuarantine time.Duration
	MaxQuarantine time.Duration
	// Lookback is how far back failures are read. It must exceed MaxQuarantine
	// so a trial's failure still finds the streak it continues.
	Lookback time.Duration
	// ReportOnly computes and logs every decision and withholds nothing: the
	// rollout mode, so an operator sees what WOULD be withheld first.
	ReportOnly bool
}

// DefaultWithholdPolicy is the policy with every default.
func DefaultWithholdPolicy() WithholdPolicy {
	return WithholdPolicy{
		Failures:      DefaultWithholdFailures,
		FailureSpan:   DefaultWithholdFailureSpan,
		MinQuarantine: DefaultWithholdMinQuarantine,
		MaxQuarantine: DefaultWithholdMaxQuarantine,
		Lookback:      DefaultWithholdLookback,
	}
}

// Validate refuses a policy that would withhold on nothing or never return.
func (p WithholdPolicy) Validate() error {
	switch {
	case p.Failures < 1:
		return fmt.Errorf("publisher: a withholding threshold of %d failures would withhold deployments that never failed", p.Failures)
	case p.FailureSpan < 0:
		return errors.New("publisher: a withholding failure span cannot be negative")
	case p.MinQuarantine <= 0:
		return errors.New("publisher: a withholding quarantine has to be positive, or a withheld deployment would flap back every cycle")
	case p.MaxQuarantine < p.MinQuarantine:
		return fmt.Errorf("publisher: the maximum quarantine %s is shorter than the minimum %s", p.MaxQuarantine, p.MinQuarantine)
	case p.Lookback <= p.MaxQuarantine:
		return fmt.Errorf("publisher: a lookback of %s must exceed the maximum quarantine %s, or a trial's failure would not find the streak it continues", p.Lookback, p.MaxQuarantine)
	case p.Lookback > MaxWithholdLookback:
		return fmt.Errorf("publisher: a lookback of %s exceeds the %s the database will read", p.Lookback, MaxWithholdLookback)
	}
	return nil
}

// quarantine is how long a streak spanning `span` withholds after its last
// failure.
func (p WithholdPolicy) quarantine(span time.Duration) time.Duration {
	return min(max(span, p.MinQuarantine), p.MaxQuarantine)
}

// Evidence is what Kaana's database says about serving, read once per cycle.
type Evidence struct {
	// Keys are the ENABLED platform keys of each asked provider. A provider
	// absent here holds none (the publisher refuses to start that way).
	Keys map[contract.ProviderSlug][]KeyEvidence
	// Bindings are the exact deployment→key bindings to enabled keys.
	Bindings map[contract.DeploymentID]KeyBinding
	// Failures are the streaks since each (deployment, key)'s last success.
	Failures []FailureStreak
}

// KeyEvidence is one enabled key's persisted state.
type KeyEvidence struct {
	KeyID string
	// Retirement is "exhausted" or "rejected" while the key has a retirement
	// on record, empty when it is usable or has never failed.
	Retirement string
	// RetiredUntil is when serving lets the key be tried again.
	RetiredUntil time.Time
	// Capacity is the latest operator-recorded evidence of each kind.
	Capacity []CapacityEvidence
}

// CapacityEvidence is one migration-0017 observation, reduced to what the
// rule needs.
type CapacityEvidence struct {
	// Kind is quota, balance or expiry.
	Kind string
	// Window is a quota's window; empty for the other kinds.
	Window     string
	ObservedAt time.Time
	// FreshUntil is when the observation stops being current. Zero is never
	// fresh: a balance nobody said is current is not read as one.
	FreshUntil time.Time
	// Empty is a quota or balance of exactly zero.
	Empty bool
	// ExpiresAt is an expiry observation's moment.
	ExpiresAt time.Time
}

// KeyBinding is one exact deployment binding.
type KeyBinding struct {
	Provider contract.ProviderSlug
	KeyID    string
}

// FailureStreak is one failure code's share of a (deployment, key) streak.
type FailureStreak struct {
	DeploymentID contract.DeploymentID
	Provider     contract.ProviderSlug
	KeyID        string
	Code         contract.ErrorCode
	Count        int
	First, Last  time.Time
}

// EvidenceSource reads Evidence. The PostgreSQL implementation lives with the
// credential store; the publisher sees only this.
type EvidenceSource interface {
	PublicationEvidence(ctx context.Context, providers []contract.ProviderSlug, since time.Time) (Evidence, error)
}

// failureClass is what a failure code says about serving the deployment.
type failureClass int

const (
	// failureIgnored says nothing about whether the deployment can serve:
	// a throttle, an overload, a timeout, the request's own fault, a filter.
	failureIgnored failureClass = iota
	// failureCredential is the provider refusing the exact key.
	failureCredential
	// failureDeployment is the provider failing this model on this account.
	failureDeployment
)

// classifyFailure is the whole vocabulary, and it is closed: a code this build
// does not name counts for nothing, so a new code can never start withholding
// deployments by accident.
//
//   - provider_billing_refused, provider_credential_invalid: the provider's
//     own report about the key (key-pools.md: a 402 retires the key).
//   - model_not_found: a model the provider still lists but will not serve on
//     this account.
//   - permission_denied: per request it is ambiguous; as an unbroken streak on
//     one deployment it is the account lacking access to the model, because a
//     per-request refusal would be interleaved with successes.
//   - provider_error: a provider 5xx or an unclassified upstream failure.
//
// Never counted, however sustained: rate_limited, provider_overloaded and
// provider_timeout (transient capacity — a throttle is not an outage, and a
// timeout is also what a long reasoning request looks like), every request
// fault, content filters and cancellations.
func classifyFailure(code contract.ErrorCode) failureClass {
	switch code {
	case contract.CodeProviderBillingRefused, contract.CodeProviderCredentialInvalid:
		return failureCredential
	case contract.CodeModelNotFound, contract.CodePermissionDenied, contract.CodeProviderError:
		return failureDeployment
	default:
		return failureIgnored
	}
}

// Decider answers Withhold for one cycle's Evidence.
type Decider struct {
	evidence Evidence
	policy   WithholdPolicy
	now      time.Time
	streaks  map[streakKey][]FailureStreak
}

type streakKey struct {
	deployment contract.DeploymentID
	keyID      string
}

// NewDecider indexes one cycle's evidence.
func NewDecider(evidence Evidence, policy WithholdPolicy, now time.Time) *Decider {
	streaks := make(map[streakKey][]FailureStreak, len(evidence.Failures))
	for _, streak := range evidence.Failures {
		key := streakKey{deployment: streak.DeploymentID, keyID: streak.KeyID}
		streaks[key] = append(streaks[key], streak)
	}
	return &Decider{evidence: evidence, policy: policy, now: now, streaks: streaks}
}

// Decide is a Withhold.
func (d *Decider) Decide(deploymentID contract.DeploymentID, slug contract.ProviderSlug) (Withholding, bool) {
	keys := d.evidence.Keys[slug]
	key, resolved := d.resolve(deploymentID, slug, keys)
	if !resolved {
		// Serving refuses this deployment as unbound whatever is published;
		// it stays listed so an operator can find the id to bind. Withheld
		// only when its provider has no key that could serve anything.
		return d.noUsableKey(keys)
	}
	if key.retired(d.now) {
		return Withholding{Reason: WithheldKeyRetired, KeyID: key.KeyID, Until: key.RetiredUntil}, true
	}
	if until, empty := key.capacityExhausted(d.now); empty {
		return Withholding{Reason: WithheldCapacityExhausted, KeyID: key.KeyID, Until: until}, true
	}
	return d.streak(deploymentID, key)
}

// resolve mirrors provider.Registry.ResolveExecution: an exact binding, else
// the provider's only enabled key, else nothing.
func (d *Decider) resolve(deploymentID contract.DeploymentID, slug contract.ProviderSlug, keys []KeyEvidence) (KeyEvidence, bool) {
	if binding, bound := d.evidence.Bindings[deploymentID]; bound {
		if binding.Provider != slug {
			return KeyEvidence{}, false
		}
		for _, key := range keys {
			if key.KeyID == binding.KeyID {
				return key, true
			}
		}
		return KeyEvidence{}, false
	}
	if len(keys) == 1 {
		return keys[0], true
	}
	return KeyEvidence{}, false
}

func (d *Decider) noUsableKey(keys []KeyEvidence) (Withholding, bool) {
	if len(keys) == 0 {
		return Withholding{}, false
	}
	var earliest time.Time
	for _, key := range keys {
		if !key.retired(d.now) {
			return Withholding{}, false
		}
		if earliest.IsZero() || key.RetiredUntil.Before(earliest) {
			earliest = key.RetiredUntil
		}
	}
	return Withholding{Reason: WithheldNoUsableKey, Until: earliest}, true
}

// streak applies the failure rule to the deployment's streak on its key.
func (d *Decider) streak(deploymentID contract.DeploymentID, key KeyEvidence) (Withholding, bool) {
	var credential, deployment streakSummary
	for _, streak := range d.streaks[streakKey{deployment: deploymentID, keyID: key.KeyID}] {
		switch classifyFailure(streak.Code) {
		case failureCredential:
			credential.add(streak)
		case failureDeployment:
			deployment.add(streak)
		}
	}
	// Fresh capacity evidence recorded after the last refusal is an operator
	// saying the account was funded again: the refusal no longer quarantines.
	// A retired key still waits out its retirement (checked before this),
	// because serving would refuse it until then regardless.
	if credential.count > 0 && key.refilledAfter(credential.last, d.now) {
		credential = streakSummary{}
	}

	var (
		counted streakSummary
		reason  WithholdReason
	)
	if credential.count > 0 {
		counted, reason = credential, WithheldCredentialRefused
	}
	if deployment.count >= d.policy.Failures && deployment.last.Sub(deployment.first) >= d.policy.FailureSpan {
		if reason == "" {
			reason = WithheldDeploymentFailing
		}
		counted.merge(deployment)
	}
	if reason == "" {
		return Withholding{}, false
	}
	until := counted.last.Add(d.policy.quarantine(counted.last.Sub(counted.first)))
	if !d.now.Before(until) {
		return Withholding{}, false
	}
	return Withholding{Reason: reason, KeyID: key.KeyID, Until: until, Failures: counted.count}, true
}

type streakSummary struct {
	count       int
	first, last time.Time
}

func (s *streakSummary) add(streak FailureStreak) {
	s.merge(streakSummary{count: streak.Count, first: streak.First, last: streak.Last})
}

func (s *streakSummary) merge(other streakSummary) {
	if other.count == 0 {
		return
	}
	if s.count == 0 || other.first.Before(s.first) {
		s.first = other.first
	}
	if s.count == 0 || other.last.After(s.last) {
		s.last = other.last
	}
	s.count += other.count
}

func (k KeyEvidence) retired(now time.Time) bool {
	return k.Retirement != "" && now.Before(k.RetiredUntil)
}

// capacityExhausted reads the latest fresh evidence. A quota counts only over
// a window that does not refill within the request's lifetime: a minute or an
// hour at zero is a throttle, not exhaustion, and throttles never withhold.
func (k KeyEvidence) capacityExhausted(now time.Time) (time.Time, bool) {
	for _, evidence := range k.Capacity {
		if !evidence.fresh(now) {
			continue
		}
		switch evidence.Kind {
		case "balance":
			if evidence.Empty {
				return evidence.FreshUntil, true
			}
		case "quota":
			if evidence.Empty && exhaustingQuotaWindow(evidence.Window) {
				return evidence.FreshUntil, true
			}
		case "expiry":
			if !evidence.ExpiresAt.IsZero() && !now.Before(evidence.ExpiresAt) {
				return evidence.FreshUntil, true
			}
		}
	}
	return time.Time{}, false
}

// refilledAfter is fresh balance or exhausting-window quota evidence, observed
// after `at`, reading more than zero.
func (k KeyEvidence) refilledAfter(at, now time.Time) bool {
	for _, evidence := range k.Capacity {
		if !evidence.fresh(now) || !evidence.ObservedAt.After(at) || evidence.Empty {
			continue
		}
		if evidence.Kind == "balance" || (evidence.Kind == "quota" && exhaustingQuotaWindow(evidence.Window)) {
			return true
		}
	}
	return false
}

func (c CapacityEvidence) fresh(now time.Time) bool {
	return !c.FreshUntil.IsZero() && now.Before(c.FreshUntil)
}

func exhaustingQuotaWindow(window string) bool {
	switch window {
	case "day", "month", "lifetime":
		return true
	default:
		return false
	}
}
