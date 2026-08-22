package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OxyHQ/Pensara/internal/contract"
)

// A provider credential is a POOL rather than a value, and this file is the
// whole of what that means.
//
// The unit here is the KEY, and it is a different axis from the deployment.
// internal/rotation takes a deployment out when the deployment is failing;
// this takes one credential out when a provider has said something about that
// credential, and the deployment goes on being served by the next key in the
// pool. Neither substitutes a model, and neither needs the other's
// authorisation: a key rotation stays inside one deployment, so it is not the
// choice among deployments that a routing policy governs.
//
// # The distinction the whole design turns on
//
// "This key has no capacity left" and "this key is momentarily throttled" are
// the same HTTP status at several providers and opposite answers. Treating
// them alike takes a healthy credential out of rotation because a provider
// asked for one fewer request this second, which is a worse failure than the
// one a pool exists to solve. So a key leaves rotation only
// when something REPORTED that it has nothing left — the provider refusing
// with its own exhaustion error, or a header the provider's mapping declares
// to mean remaining credits reading zero. A state nobody could determine is
// `unknown`, and unknown never disables anything.
//
// The reasoning is OmniRoute's (MIT, https://github.com/diegosouzapw/OmniRoute,
// docs/OMNIROUTE_PROVIDER_FAILOVER.md and docs/OMNIROUTE_QUOTA_TELEMETRY.md);
// the code is this repository's.

// DefaultKeyRetirement is how long a key stays out of rotation after a provider
// reported it spent or refused it.
//
// It is a flat window rather than a doubling backoff, because the two things
// that put a key here are scheduled rather than open-ended: a quota resets on
// the provider's own cycle, and a revoked key comes back when an operator
// rotates it. A backoff models an outage of unknown length, which is what the
// deployment breaker is for.
//
// A key that never returned would be worse than the problem: a process that
// has retired every credential it holds serves nothing until somebody restarts
// it, and one failing round trip every quarter of an hour is the price of
// never being in that state.
const DefaultKeyRetirement = 15 * time.Minute

/* -------------------------------------------------------------------------- */
/*  What a failure says about the credential that produced it                 */
/* -------------------------------------------------------------------------- */

// CredentialVerdict is what an upstream failure says about the KEY that was
// used, which is a different question from what it says about the deployment
// (AttributableCategory) or about the customer (the contract's error code).
//
// One failure answers all three, and the three answers do not agree: a refused
// credential is terminal for the key, attributable to the deployment, and
// non-retryable for the customer.
type CredentialVerdict string

const (
	// CredentialHealthy means the failure says nothing about the key. Timeouts,
	// network failures, provider 5xx and rate limits all land here: the key
	// stays in rotation, and whether the REQUEST is retried anywhere is the
	// deployment lane's decision, not this one's.
	CredentialHealthy CredentialVerdict = "healthy"
	// CredentialExhausted means the provider reported that this key's account
	// has no capacity left. It is the one verdict that both retires a key and
	// moves the request to the next one, because the next key is a different
	// account with its own capacity and is very likely to serve it.
	CredentialExhausted CredentialVerdict = "exhausted"
	// CredentialRejected means the provider refused this credential: revoked,
	// invalid, or lacking access somebody has to grant it.
	//
	// It retires the key and does NOT move the request. If one key is refused,
	// the likeliest explanations are an operator error and a provider-side auth
	// failure — and under the second, every remaining key is refused
	// identically, so walking the pool multiplies one failure into N upstream
	// calls and retires the whole pool on a blip.
	CredentialRejected CredentialVerdict = "rejected"
	// CredentialRequestFault means the request is what was refused. No key can
	// fix it, so it retires nothing and is retried nowhere: the next credential
	// would be refused identically, and one customer error would have become
	// several upstream calls.
	CredentialRequestFault CredentialVerdict = "request_fault"
)

// CredentialVerdictFor classifies a failure an adapter has ALREADY classified.
//
// It reads the adapter's contract code rather than an HTTP status, for the same
// reason the adapters do: a 429 from a spent monthly quota and a 429 from a
// burst limit are one status and opposite answers, and the adapter is the only
// layer that saw the provider's own error type. Re-deriving it here would be a
// second, worse classification of the same failure.
//
// Every unrecognised failure is CredentialHealthy. That default is the rule
// this file exists for: a state nobody could determine is not exhaustion, and
// guessing exhaustion from an ambiguous signal disables working credentials.
func CredentialVerdictFor(err error) CredentialVerdict {
	var upstream ErrUpstream
	if !errors.As(err, &upstream) {
		// A cancellation, a sink failure, an error no adapter classified. None
		// of them is a statement about the credential.
		return CredentialHealthy
	}

	switch upstream.Code {
	case contract.CodeProviderCredentialInvalid:
		return CredentialRejected

	case contract.CodeProviderBillingRefused:
		// The upstream declining to bill the account behind this key IS the
		// report of zero remaining capacity. It is the strongest exhaustion
		// signal there is, because it comes from the provider about the exact
		// credential that was sent.
		return CredentialExhausted

	case contract.CodeQuotaExceeded, contract.CodeInsufficientBalance, contract.CodeSpendingLimitExceeded:
		// These name the CUSTOMER's ceiling. Retiring a key here would disable
		// a working credential because a customer ran out of money, which is
		// the mirror image of the mistake this file is about.
		return CredentialRequestFault

	case contract.CodeInvalidRequest, contract.CodeModelNotFound, contract.CodeUnsupportedModality,
		contract.CodeContextLengthExceeded, contract.CodeRequestTooLarge, contract.CodeOutputLimitExceeded,
		contract.CodeUpstreamContentFiltered, contract.CodePermissionDenied, contract.CodeAuthenticationFailed:
		return CredentialRequestFault

	case contract.CodeRateLimited, contract.CodeProviderTimeout, contract.CodeProviderOverloaded,
		contract.CodeProviderError:
		return CredentialHealthy
	}
	return CredentialHealthy
}

// Throttled reports whether a failure is a provider throttle — the transient
// refusal that clears by itself.
//
// It is separate from the verdict because a throttle is the one healthy-key
// failure that a DIFFERENT key can survive, and only when the operator has
// said the pool's keys sit on separate provider accounts. See
// KeyPolicy.OnSeparateAccounts.
func Throttled(err error) bool {
	var upstream ErrUpstream
	if !errors.As(err, &upstream) {
		return false
	}
	return upstream.Category == contract.UpstreamRateLimit
}

/* -------------------------------------------------------------------------- */
/*  Quota telemetry                                                           */
/* -------------------------------------------------------------------------- */

// QuotaState is what a source reported about a key's remaining capacity.
//
// The states are separate because collapsing them is the failure mode: an
// operator reading "exhausted" believes a provider said so, and a build that
// also says "exhausted" when it simply could not tell has disabled a working
// credential and reported it as a fact.
type QuotaState string

const (
	// QuotaHealthy means a source reported usable remaining capacity.
	QuotaHealthy QuotaState = "healthy"
	// QuotaExhausted means a source reported ZERO remaining capacity. It is the
	// only state that takes a key out of rotation.
	QuotaExhausted QuotaState = "exhausted"
	// QuotaUnavailable means a supported source was there and could not be
	// read — a declared header carrying something that is not a number. It is
	// not exhaustion; nothing was reported.
	QuotaUnavailable QuotaState = "unavailable"
	// QuotaUnknown means no supported source exists for this provider. It is
	// the default, and it disables nothing.
	QuotaUnknown QuotaState = "unknown"
)

// QuotaSignal is what a response header MEANS, declared rather than inferred.
//
// A header name is never read for its meaning. `x-ratelimit-remaining` looks
// like a quota signal at every provider and is a BURST limit at most of them,
// so a build that assumed generic names would retire a healthy key every time
// a provider throttled it — the exact confusion this package refuses to make.
type QuotaSignal string

const (
	// SignalRemainingCredits is a count of the account's remaining spendable
	// capacity. Zero means exhausted; it is the only signal that can retire a
	// key. A rate-limit remaining count is NOT this signal.
	SignalRemainingCredits QuotaSignal = "remaining_credits"
	// SignalResetAt is when the provider says the capacity returns, as a Unix
	// timestamp in seconds. It refines how long a retirement lasts and can
	// never cause one.
	SignalResetAt QuotaSignal = "reset_at"
)

// QuotaHeaders is one provider's declared mapping from a response header to
// what that header means.
//
// It lives in the adapter package beside the code that speaks the provider's
// wire format, not in operator configuration, for the same reason the Anthropic
// adapter pins its API version in code: an operator who mapped a rate-limit
// header onto SignalRemainingCredits would retire every key the moment the
// provider throttled one, and the mistake would be invisible until the pool
// was empty.
type QuotaHeaders map[string]QuotaSignal

// QuotaObservation is what one response said about the key that produced it.
type QuotaObservation struct {
	State QuotaState
	// ResetAt is when the provider says the capacity returns. Zero when no
	// declared header carried it.
	ResetAt time.Time
}

// Read applies the mapping to a response's headers.
//
// An absent header is QuotaUnknown and never QuotaExhausted: the difference
// between "the provider said zero" and "the provider said nothing" is the
// whole point of the distinction.
func (m QuotaHeaders) Read(header http.Header) QuotaObservation {
	observation := QuotaObservation{State: QuotaUnknown}
	if len(m) == 0 || header == nil {
		return observation
	}

	for name, signal := range m {
		raw := strings.TrimSpace(header.Get(name))
		if raw == "" {
			continue
		}
		switch signal {
		case SignalRemainingCredits:
			remaining, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				// A declared source that answered something unreadable. That is
				// a source failing, not a key with nothing left.
				observation.State = QuotaUnavailable
				continue
			}
			if remaining <= 0 {
				observation.State = QuotaExhausted
				continue
			}
			if observation.State != QuotaExhausted {
				observation.State = QuotaHealthy
			}
		case SignalResetAt:
			seconds, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || seconds <= 0 {
				continue
			}
			observation.ResetAt = time.Unix(seconds, 0).UTC()
		}
	}
	return observation
}

/* -------------------------------------------------------------------------- */
/*  The pool                                                                  */
/* -------------------------------------------------------------------------- */

// KeyRetirement is why a key is out of rotation.
type KeyRetirement string

const (
	// KeyExhausted is a key whose account reported no capacity left.
	KeyExhausted KeyRetirement = "exhausted"
	// KeyRejected is a key the provider refused.
	KeyRejected KeyRetirement = "rejected"
)

// Key is one credential leased from a pool.
//
// Its secret is unexported and reachable only through Secret(), so a Key cannot
// be constructed anywhere but here: an adapter cannot fabricate one and send a
// credential the pool has retired.
type Key struct {
	// Position is the key's 1-based place in the declared list, and is its
	// whole identity outside this package. It is a position rather than any
	// function of the secret, so nothing derived from a credential is ever
	// written to a log or a health projection.
	Position int
	secret   string
}

// Secret is the credential itself. Every call site is an adapter applying
// authentication at send time, or redacting the exact bytes it sent out of text
// bound for a customer.
func (k Key) Secret() string { return k.secret }

// KeyPolicy is the operator-settable half of a pool's behaviour.
type KeyPolicy struct {
	// Retirement is how long a retired key stays out. Zero takes
	// DefaultKeyRetirement.
	Retirement time.Duration
	// OnSeparateAccounts states that the keys in this pool belong to DIFFERENT
	// provider accounts, which is a fact only the operator who provisioned them
	// knows.
	//
	// It governs exactly one behaviour: whether a THROTTLE may be retried on
	// another key. Keys of one account share that account's rate limit, so
	// rotating into it would hammer a provider that has just asked for less
	// traffic; keys of separate accounts have separate limits, and the next one
	// can serve the request. Pensara cannot tell the two apart, so it does not
	// guess — false, the default, never rotates on a throttle.
	OnSeparateAccounts bool
}

// KeyPool holds every credential this process has for one provider, and which
// of them are usable right now.
type KeyPool struct {
	provider contract.ProviderSlug
	policy   KeyPolicy
	signals  QuotaHeaders

	mu   sync.Mutex
	keys []*pooledKey
}

type pooledKey struct {
	position int
	secret   string
	// retiredUntil is the moment this key returns to rotation. Zero means it
	// never left.
	retiredUntil time.Time
	reason       KeyRetirement
}

// NewKeyPool builds a pool from the credentials declared for one provider.
//
// It refuses what would present as a working pool and behave as a broken one: a
// blank entry (an operator's trailing separator, which would send an empty
// credential and be refused), and a duplicate (a copy-paste that looks like two
// keys, exhausts as one, and halves the pool the first time it is spent).
func NewKeyPool(slug contract.ProviderSlug, secrets []string, policy KeyPolicy, signals QuotaHeaders) (*KeyPool, error) {
	if policy.Retirement <= 0 {
		policy.Retirement = DefaultKeyRetirement
	}
	pool := &KeyPool{provider: slug, policy: policy, signals: signals}

	seen := make(map[string]int, len(secrets))
	for index, secret := range secrets {
		trimmed := strings.TrimSpace(secret)
		if trimmed == "" {
			return nil, fmt.Errorf("provider: %s declares an empty credential at position %d", slug, index+1)
		}
		if first, duplicate := seen[trimmed]; duplicate {
			// Named by POSITION. The credential itself never enters an error,
			// including the one that reports it is wrong.
			return nil, fmt.Errorf("provider: %s declares the same credential at positions %d and %d, which is one key wearing two names", slug, first, index+1)
		}
		seen[trimmed] = index + 1
		pool.keys = append(pool.keys, &pooledKey{position: index + 1, secret: trimmed})
	}
	return pool, nil
}

// Configured reports whether the pool holds any credential at all. A pool with
// none is a distinct state from one whose keys are all retired: the first is an
// operator gap, the second is a provider having spent them.
func (p *KeyPool) Configured() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.keys) > 0
}

// Begin starts one request's walk through the pool.
func (p *KeyPool) Begin() *KeyAttempt {
	return &KeyAttempt{pool: p, tried: make(map[int]bool, 2)}
}

// Retire takes a key out of rotation until a moment.
//
// A `until` in the past or the zero time takes the policy's retirement window.
// A provider that said when the capacity returns is believed over the window,
// because the window is a guess and the provider's answer is not.
func (p *KeyPool) Retire(key Key, reason KeyRetirement, at time.Time, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, candidate := range p.keys {
		if candidate.position != key.Position {
			continue
		}
		if !until.After(at) {
			until = at.Add(p.policy.Retirement)
		}
		candidate.retiredUntil = until
		candidate.reason = reason
		return
	}
}

// Observe applies a response's quota telemetry to the key that produced it.
//
// It runs on EVERY response, including successful ones, because the point of a
// proactive signal is to skip a key that is about to refuse rather than to
// learn about it from the refusal. Only QuotaExhausted does anything: healthy,
// unavailable and unknown all leave the key exactly as it was.
func (p *KeyPool) Observe(key Key, header http.Header, at time.Time) {
	if observation := p.signals.Read(header); observation.State == QuotaExhausted {
		p.Retire(key, KeyExhausted, at, observation.ResetAt)
	}
}

// NoUsableCredential is the failure for a request that cannot be sent at all,
// classified so that it means the same thing as every other upstream failure to
// the breaker, to failover and to the customer.
//
// Two different situations reach it and they are not the same operator problem,
// so they do not produce the same error:
//
//   - Nothing configured. Non-retryable — no retry creates a credential — and
//     attributable, so the deployment leaves rotation and a same-model failover
//     to a deployment whose provider IS configured is permitted.
//   - Everything retired. Retryable, carrying the moment the earliest key
//     returns, because that is a fact rather than a number chosen to look
//     reasonable.
func (p *KeyPool) NoUsableCredential(at time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.keys) == 0 {
		return ErrUpstream{
			Code:        contract.CodeProviderCredentialInvalid,
			Category:    contract.UpstreamAuthentication,
			Detail:      fmt.Sprintf("no credential is configured for %s", p.provider),
			Passthrough: &contract.ProviderErrorPassthrough{Provider: p.provider},
		}
	}

	category := contract.UpstreamQuota
	earliest := time.Time{}
	for _, key := range p.keys {
		if key.reason == KeyRejected {
			// A refused credential is the one an operator has to act on, so it
			// is what the category names when both kinds are present.
			category = contract.UpstreamAuthentication
		}
		if earliest.IsZero() || key.retiredUntil.Before(earliest) {
			earliest = key.retiredUntil
		}
	}

	failure := ErrUpstream{
		Code:     contract.CodeDeploymentUnavailable,
		Category: category,
		Detail: fmt.Sprintf("every credential this build holds for %s is out of rotation (%d declared)",
			p.provider, len(p.keys)),
		Passthrough: &contract.ProviderErrorPassthrough{Provider: p.provider},
	}
	if wait := earliest.Sub(at); wait > 0 {
		failure.RetryAfterMs = int(wait.Milliseconds())
	}
	return failure
}

/* -------------------------------------------------------------------------- */
/*  One request's walk through the pool                                       */
/* -------------------------------------------------------------------------- */

// KeyAttempt is one request's walk through a pool.
//
// It exists so that no key is used twice for one request. That is what bounds
// key failover without a configured ceiling: a request can make at most as many
// upstream calls as the pool has keys, and the bound is a property of the walk
// rather than a number somebody chose.
type KeyAttempt struct {
	pool  *KeyPool
	tried map[int]bool
	// throttleRotations counts the rotations spent on a transient throttle,
	// which retires nothing and would otherwise repeat on every request.
	throttleRotations int
}

// Next leases the next usable credential this request has not already used.
//
// Keys are served in the order they were declared rather than round-robin. A
// pool exists because keys have separate capacity, and spending one key before
// moving to the next is what makes exhaustion an event with a next key waiting;
// spreading load over all of them arrives at every key being nearly spent at
// once.
func (a *KeyAttempt) Next(at time.Time) (Key, bool) {
	a.pool.mu.Lock()
	defer a.pool.mu.Unlock()

	for _, key := range a.pool.keys {
		if a.tried[key.position] {
			continue
		}
		if key.retiredUntil.After(at) {
			continue
		}
		a.tried[key.position] = true
		return Key{Position: key.position, secret: key.secret}, true
	}
	return Key{}, false
}

// AllowThrottleRotation reports whether a transient throttle may be retried on
// another key, and records that it was.
//
// It is true at most once per request, and only when the operator declared that
// this pool's keys sit on separate provider accounts. Both halves are the
// bound: without the declaration a rotation would walk into the same account's
// rate limit, and without the ceiling a throttled provider would receive one
// call per key per request for as long as the throttle lasted — retiring
// nothing, so repeating in full on the next request too.
func (a *KeyAttempt) AllowThrottleRotation() bool {
	if !a.pool.policy.OnSeparateAccounts || a.throttleRotations > 0 {
		return false
	}
	a.throttleRotations++
	return true
}

/* -------------------------------------------------------------------------- */
/*  The walk itself                                                           */
/* -------------------------------------------------------------------------- */

// CredentialedSender is the half of an adapter a pool walk needs: how to send
// one call with one credential, and how that provider says a request failed.
//
// It exists so the rules below are ONE implementation rather than one per
// adapter. Which failure retires a key, which one moves the request, and which
// one is retried nowhere is a platform decision with money and working
// credentials attached — an adapter that restated it would be free to restate
// it differently, and the two would be discovered to disagree by a customer.
type CredentialedSender interface {
	// Send performs the upstream call with one leased credential. Applying the
	// credential at send time is the adapter's, because only it knows the
	// header its provider authenticates with.
	Send(ctx context.Context, call *Call, key Key) (*http.Response, error)
	// Refuse classifies a non-2xx response from this provider's OWN error
	// vocabulary, reads what it needs from the body, and CLOSES it.
	//
	// It closes rather than leaving it to the walk because the walk never looks
	// at a body: everything it decides comes from the classification, and a
	// response it is holding open across a rotation is a leaked connection per
	// failed attempt.
	Refuse(response *http.Response, key Key) error
	// TransportFailure classifies a failure that happened before any response
	// arrived.
	TransportFailure(ctx context.Context, err error) error
}

// Walk sends a call with the first credential that can serve it, rotating on
// the failures that say something about a credential and on nothing else. It
// returns the response that will be read and the key that produced it; the
// caller closes that response.
//
// Everything here happens BEFORE the response body is read, and that is the
// design rather than a convenience: not one byte has reached the customer, so
// moving to another credential replaces the request instead of splicing two
// answers together. Once a body is being read the request is committed to the
// key that opened it, which is why a failure arriving mid-stream — after a 200,
// where every streaming protocol has a spelling for one — rotates nothing.
//
// A rotation never leaves the deployment: same provider, same endpoint, same
// upstream model, a different credential. It is not the choice among
// deployments that a routing policy governs, so it needs no authorisation from
// one, and it emits no route switch because nothing about the customer's route
// changed.
func Walk(ctx context.Context, pool *KeyPool, call *Call, sender CredentialedSender) (*http.Response, Key, error) {
	attempt := pool.Begin()
	// refused is the last classified failure this request received. When the
	// pool runs out, THAT is what the customer is told — the provider's own
	// answer about the last credential tried, with its passthrough — rather
	// than a synthesized "nothing left to try", which would replace a diagnosis
	// with a vaguer restatement of it.
	var refused error

	for {
		now := time.Now()
		key, leased := attempt.Next(now)
		if !leased {
			if refused != nil {
				return nil, Key{}, refused
			}
			// Not one key could be leased, so no provider ever saw this
			// request: either nothing is configured, or every credential was
			// retired by an earlier one.
			return nil, Key{}, pool.NoUsableCredential(now)
		}

		response, err := sender.Send(ctx, call, key)
		if err != nil {
			// A transport failure says nothing about the credential: the
			// request never reached the provider, so nobody refused it.
			return nil, Key{}, sender.TransportFailure(ctx, err)
		}

		// Applied to every response, successful ones included: a proactive
		// quota signal earns its place by skipping a key that is about to
		// refuse, not by explaining a refusal that already happened.
		pool.Observe(key, response.Header, now)

		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return response, key, nil
		}

		failure := sender.Refuse(response, key)

		switch CredentialVerdictFor(failure) {
		case CredentialExhausted:
			// The provider reported that this key's account has nothing left.
			// The next key is a different account, so the request moves and the
			// customer never learns this happened.
			pool.Retire(key, KeyExhausted, now, time.Time{})
			refused = failure

		case CredentialRejected:
			// The credential was refused. The key leaves rotation so no later
			// request pays for it again, and this request does NOT walk the
			// rest of the pool: under a provider-side auth failure every
			// remaining key is refused identically, so walking would multiply
			// one failure into a call per key and retire the whole pool on a
			// blip.
			pool.Retire(key, KeyRejected, now, time.Time{})
			return nil, Key{}, failure

		case CredentialRequestFault:
			// The request is what was refused, and it would be refused
			// identically by every other credential. Retried nowhere.
			return nil, Key{}, failure

		case CredentialHealthy:
			// The failure says nothing about this key, so nothing is retired.
			// A throttle is the one case another key could survive, and only
			// where the operator has stated the pool's keys sit on separate
			// provider accounts — otherwise the next key shares the limit that
			// has just been reached.
			if Throttled(failure) && attempt.AllowThrottleRotation() {
				refused = failure
				continue
			}
			return nil, Key{}, failure

		default:
			return nil, Key{}, failure
		}
	}
}

/* -------------------------------------------------------------------------- */
/*  Health projection                                                         */
/* -------------------------------------------------------------------------- */

// KeyHealth is one credential's state, with nothing derived from its secret.
type KeyHealth struct {
	Position int `json:"position"`
	// State is `usable`, or the KeyRetirement that took it out.
	State string `json:"state"`
	// RetiredUntil is when it returns, present only while it is out.
	RetiredUntil *contract.Timestamp `json:"retiredUntil,omitempty"`
}

// KeyPoolHealth is the operator-facing projection of a pool.
//
// It carries counts and positions and no credential, not even a truncated hash
// of one: a fingerprint of a secret confirms a guessed secret, and nothing here
// needs one.
type KeyPoolHealth struct {
	Declared int         `json:"declared"`
	Usable   int         `json:"usable"`
	Keys     []KeyHealth `json:"keys"`
}

// Projection reports the pool's state for the health surface.
func (p *KeyPool) Projection(at time.Time) KeyPoolHealth {
	p.mu.Lock()
	defer p.mu.Unlock()

	health := KeyPoolHealth{Declared: len(p.keys), Keys: make([]KeyHealth, 0, len(p.keys))}
	for _, key := range p.keys {
		projected := KeyHealth{Position: key.position, State: "usable"}
		if key.retiredUntil.After(at) {
			projected.State = string(key.reason)
			retiredUntil := contract.NewTimestamp(key.retiredUntil)
			projected.RetiredUntil = &retiredUntil
		} else {
			health.Usable++
		}
		health.Keys = append(health.Keys, projected)
	}
	sort.Slice(health.Keys, func(a, b int) bool { return health.Keys[a].Position < health.Keys[b].Position })
	return health
}
