// Package provider defines the contract every upstream adapter satisfies.
//
// This interface is the point of the data plane. Everything else in Kaana is
// plumbing around it: the HTTP surface exists to hand an adapter a normalized
// request, and the executor exists to turn what an adapter reports into a
// stream and a usage record. Adding a provider must therefore mean writing one
// implementation of Adapter and one fake upstream for the conformance harness —
// never touching the executor, the stream framing or the receipt shape.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// Adapter translates the normalized inference contract into one upstream
// provider's wire protocol and back.
//
// Five methods, and each exists because the concern it names has a different
// lifetime from the others:
//
//   - Provider names the slug every event and usage record attributes the work
//     to. It comes from the adapter rather than from its registration site so a
//     mis-registration cannot mislabel a receipt.
//   - APIFormats declares the request families this adapter executes. It is a
//     static fact about the adapter, not a per-request decision, so the
//     registry can refuse an adapter that declares none and the executor can
//     refuse a request family before Translate is ever reached: a text adapter
//     is never handed a transcription or session request to guess about.
//   - Translate is pure. A request this provider cannot express must be refused
//     BEFORE anything is spent upstream, and a pure translation is testable
//     without a network, which is what makes that refusal cheap to cover.
//   - Stream is execution, streaming, cancellation and usage measurement
//     together, because they share one lifetime: the units measured are only
//     correct if they come from the same read loop that saw the last frame
//     before a cancellation. A separate Usage() method would invite being
//     called on a stream that ended differently from the one it measured.
//   - Health must be answerable without a customer request — it feeds the
//     internal status surface — so it cannot be folded into Stream. It is
//     deliberately NOT what returns a deployment to rotation: that is one real
//     customer request through a half-open breaker (internal/rotation), because
//     a probe of the provider's model listing proves it answers some other
//     request than the one it is failing.
//
// Everything an adapter does NOT do is as deliberate. It does not allocate
// request or generation ids, assign sequence numbers, decide terminality, emit
// the done or error event, resolve a model reference to an upstream model id,
// or apply routing policy. Those are one implementation in the executor rather
// than one per provider, which is the difference between adding a provider and
// re-deriving the contract each time.
type Adapter interface {
	Provider() contract.ProviderSlug
	APIFormats() []contract.APIFormat
	Translate(request *contract.Request, route Route) (*Call, error)
	Stream(ctx context.Context, call *Call, out Emitter, credentials *KeyPool) (Outcome, error)
	Health(ctx context.Context) Health
}

// PlatformCredentialSource is implemented by production adapters backed by
// Kaana's encrypted platform credential store. Keeping it separate preserves
// the provider adapter contract for test and non-platform implementations.
type PlatformCredentialSource interface {
	PlatformCredentials() *KeyPool
}

// Route is what the deployment inventory resolved for this request: which
// concrete endpoint serves it, and what the upstream calls the model.
//
// UpstreamModelID has no representation anywhere in the published contract —
// the catalogue's deployment descriptor carries no field for a provider's own
// model identifier. The mapping is therefore Kaana's, and it lives in the
// inventory rather than inside each adapter so two adapters for one provider
// cannot disagree about it.
type Route struct {
	ScopedExecution *contract.ScopedExecutionAudience
	DeploymentID    contract.DeploymentID
	Provider        contract.ProviderSlug
	ModelReference  contract.ModelReference
	UpstreamModelID string
	Regions         []contract.Region
	// AcceptedParameters is the deployment's published statement of which
	// caller controls its upstream accepts (inventory `observed`). Nil is
	// UNKNOWN and refuses nothing; a present set lets Translate refuse a
	// control the upstream would reject before anything is spent. It can only
	// refuse: nothing in it is ever sent or defaulted.
	AcceptedParameters *[]RequestParameter
	// CustomerProviderCredential is the exact non-secret binding Oxy signed for
	// this route. Nil means the provider call uses Kaana's platform pool.
	CustomerProviderCredential *contract.CustomerProviderCredential
}

// Call is a translated, ready-to-send upstream request.
//
// It carries no credential. The adapter holds its own, applies it when it
// builds the HTTP request, and nothing that could be logged, echoed into an
// error or written to a usage record ever holds one.
// ScopedCredentialAttempt narrows an already-authorized call to one exact key
// and a durable claim immediately before the exchange. It is not authorization.
// Only the executor's verified scoped-envelope path may populate this field.
type ScopedCredentialAttempt struct {
	KeyID string
	Claim func(context.Context) error `json:"-"`
}

type Call struct {
	ScopedAttempt *ScopedCredentialAttempt `json:"-"`

	Decisions *contract.DecisionInput

	// RequestID and Route identify credential-attempt telemetry. Neither contains
	// credential material.
	RequestID contract.RequestID
	Route     Route
	// CredentialAttempts numbers this request's credential attempts across
	// EVERY walk it makes. The durable record is keyed by (request,
	// deployment, index), and a same-route retry is a fresh walk on the same
	// deployment: a walk-local index would restart at zero and name the retry
	// with the first attempt's identity. A caller that walks more than once per
	// request shares one sequence across its calls; nil numbers this walk
	// alone, which is correct only for a single walk per request.
	CredentialAttempts *CredentialAttemptSequence
	// Method and URL are recorded so a failure can name the endpoint that
	// failed without reconstructing it from adapter internals.
	Method string
	URL    string
	// Body is the upstream request body, already encoded.
	Body []byte
	// Header carries only non-secret headers. The adapter adds authentication
	// at send time.
	Header http.Header
	// Stream records whether the customer asked for a streamed response, since
	// providers express it in the body and Kaana has to know without re-reading
	// it.
	Stream bool
	// AudioMediaType is the media type of the audio this call asks the upstream
	// to produce, for a response that does not name it. It is never sent.
	AudioMediaType string
}

// Outcome is what an adapter measured, and it is returned even when Stream
// fails.
//
// It is a value rather than a pointer for exactly that reason: a partial stream
// is a settlement case, so an adapter that returned nothing on cancellation
// would make an exact refund impossible, and a nil pointer is the easiest way
// to accidentally return nothing.
type Outcome struct {
	Decisions []contract.DecisionAnswer

	Embedding *EmbeddingResult
	// Units measured so far. Each unit appears at most once, as a total.
	Units []contract.UsageQuantity
	// UsageSource distinguishes what the provider reported from what Kaana
	// counted from what it had to reconstruct. An estimate indistinguishable
	// from a reported number is one nobody can reconcile later.
	UsageSource contract.UsageSource
	// FinishReason is why generation stopped, as the provider described it.
	FinishReason contract.FinishReason
	// TimeToFirstToken is zero when no output was produced.
	TimeToFirstToken time.Duration
	// KeyID names the pool key this attempt spent, and KeyClass what the
	// operator said that key costs. They exist so the operator log can answer
	// "which credential paid for this", which is the question a budget is built
	// on and which `DeploymentID` cannot answer: one deployment is served by a
	// pool, and the pool is the thing that runs out.
	//
	// They are operator numbers in the same sense the cost is, and they travel
	// inside the same Record, so the containment gate that keeps cost away from
	// the customer covers them without a second mechanism.
	KeyID    string
	KeyClass KeyClass
	// ProviderReportedCost is an exact upstream billing fact, when the provider
	// returns one. The owned type keeps all monetary arithmetic in providercost.
	ProviderReportedCost *providercost.Money
}

// EmbeddingResult is normalized non-streaming vector output. It remains an
// adapter result until the executor stamps request and canonical model identity.
type EmbeddingResult struct {
	Dimension   int
	Data        []contract.EmbeddingVector
	InputTokens int
	TotalTokens int
}

// HealthStatus is the coarse state of an adapter's upstream.
type HealthStatus string

const (
	// HealthOK means the upstream answered a probe.
	HealthOK HealthStatus = "ok"
	// HealthDegraded means the upstream answered, but not well enough to route
	// to by preference.
	HealthDegraded HealthStatus = "degraded"
	// HealthUnavailable means the probe failed.
	HealthUnavailable HealthStatus = "unavailable"
	// HealthUnconfigured means no credential is configured for this adapter.
	// It is a distinct state from unavailable on purpose: an operator reading
	// "unavailable" goes looking at the provider, and the answer is here.
	HealthUnconfigured HealthStatus = "unconfigured"
)

// Health is the customer-safe projection of an adapter's state. It carries no
// credential, no upstream URL and no internal route id.
type Health struct {
	Provider  contract.ProviderSlug `json:"provider"`
	Status    HealthStatus          `json:"status"`
	CheckedAt contract.Timestamp    `json:"checkedAt"`
	LatencyMs *int                  `json:"latencyMs,omitempty"`
	// Detail is a short operator-facing note. It goes through the contract's
	// own redaction, because a probe failure is one of the places an upstream
	// echoes a credential back.
	Detail string `json:"detail,omitempty"`
	// Credentials is the state of this provider's key pool: how many are
	// declared, how many can be used right now, and which of them are out and
	// until when.
	//
	// It carries positions and states and nothing derived from a secret. It is
	// here because a pool draining towards empty is otherwise invisible until
	// the request that finds it empty — the same argument that puts snapshot
	// staleness on this surface.
	Credentials *KeyPoolHealth `json:"credentials,omitempty"`
}

// Emitter is how an adapter reports semantic output.
//
// It exists so no adapter can get the framing wrong. `requestId`, `sequence`
// and `schemaVersion` are stamped here, once, rather than by each adapter —
// which removes the entire class of bug where one provider's events are
// unattributable or arrive with a repeated sequence. Terminal events are
// deliberately absent: `done`, `error` and `route_switch` are the executor's,
// because an adapter does not know whether a failure ends the request or is
// about to be retried elsewhere.
type Emitter interface {
	// Start reports what was actually resolved. Exactly one Start per stream,
	// and it must precede every other event.
	Start(resolved contract.ModelReference, at time.Time) error
	// Delta reports a chunk of output on one channel.
	Delta(outputIndex int, channel contract.DeltaChannel, text string) error
	// ToolCall reports a tool call being streamed.
	ToolCall(call ToolCallDelta) error
	// Usage reports metered units for the request so far. Units only — a cost
	// quoted by the data plane would be a second, unauthoritative answer to a
	// question the ledger already owns.
	Usage(units []contract.UsageQuantity, source contract.UsageSource) error
}

// ToolCallDelta is one increment of a streamed tool call.
type ToolCallDelta struct {
	ID string
	// Name is set on the first increment of a call.
	Name string
	// ArgumentsDelta accumulates; it is not valid JSON until Complete.
	ArgumentsDelta string
	Complete       bool
}

// Registrant is what the registry holds: one provider implementation under
// its own slug. It is exactly one of Adapter (one-shot requests) or
// RealtimeAdapter (sessions). A slug resolves to one implementation, so a type
// that claimed both would make which code serves a deployment depend on which
// question happened to be asked.
type Registrant interface {
	Provider() contract.ProviderSlug
	Health(ctx context.Context) Health
}

// Registry holds the adapters this process can route to.
//
// It keys on the adapter's OWN slug rather than on a name supplied at
// registration, so a typo cannot produce a provider that serves requests under
// one name and reports usage under another.
type Registry struct {
	mu               sync.RWMutex
	adapters         map[contract.ProviderSlug]Adapter
	sessions         map[contract.ProviderSlug]RealtimeAdapter
	bindings         map[contract.DeploymentID]CredentialBinding
	bindingsRequired bool
}

type CredentialBinding struct {
	DeploymentID contract.DeploymentID
	Provider     contract.ProviderSlug
	KeyID        string
}

// NewRegistry builds a registry, refusing duplicates, invalid slugs, and an
// implementation that is not exactly one of the two adapter kinds.
func NewRegistry(registrants ...Registrant) (*Registry, error) {
	registry := &Registry{
		adapters: make(map[contract.ProviderSlug]Adapter, len(registrants)),
		sessions: make(map[contract.ProviderSlug]RealtimeAdapter),
		bindings: make(map[contract.DeploymentID]CredentialBinding),
	}
	for _, registrant := range registrants {
		slug := registrant.Provider()
		if !slug.Valid() {
			return nil, fmt.Errorf("provider: %T reports slug %q, which is not a provider slug", registrant, slug)
		}
		if registry.serves(slug) {
			return nil, fmt.Errorf("provider: two adapters claim the slug %q", slug)
		}
		adapter, oneShot := registrant.(Adapter)
		session, realtime := registrant.(RealtimeAdapter)
		switch {
		case oneShot && realtime:
			return nil, fmt.Errorf("provider: %T for %s is both a request adapter and a realtime session adapter; a slug resolves to exactly one", registrant, slug)
		case oneShot:
			formats := adapter.APIFormats()
			if len(formats) == 0 {
				return nil, fmt.Errorf("provider: %s declares no request family it can execute", slug)
			}
			for _, format := range formats {
				if !format.Valid() {
					return nil, fmt.Errorf("provider: %s declares %q, which is not an api format", slug, format)
				}
			}
			registry.adapters[slug] = adapter
		case realtime:
			kinds := session.RealtimeSessionKinds()
			if len(kinds) == 0 {
				return nil, fmt.Errorf("provider: %s declares no realtime session kind it can open", slug)
			}
			for _, kind := range kinds {
				if !kind.Valid() {
					return nil, fmt.Errorf("provider: %s declares %q, which is not a realtime session kind", slug, kind)
				}
			}
			registry.sessions[slug] = session
		default:
			return nil, fmt.Errorf("provider: %T for %s executes neither requests nor realtime sessions", registrant, slug)
		}
	}
	return registry, nil
}

func (r *Registry) serves(slug contract.ProviderSlug) bool {
	_, oneShot := r.adapters[slug]
	_, realtime := r.sessions[slug]
	return oneShot || realtime
}

func (r *Registry) registrant(slug contract.ProviderSlug) (Registrant, bool) {
	if adapter, ok := r.adapters[slug]; ok {
		return adapter, true
	}
	if session, ok := r.sessions[slug]; ok {
		return session, true
	}
	return nil, false
}

// ResolveExecution returns an adapter and its exact platform credential view
// from one registry generation. One read lock prevents reload from pairing an
// old adapter with bindings from a different generation.
//
// A platform deployment resolves to exactly one key, by the first rule that
// applies:
//
//  1. An exact deployment binding. It is final: a binding to a retired key,
//     or one naming a different provider, never falls back to anything.
//  2. With no exact binding, the provider's key, when the provider holds
//     exactly one. A key belongs to its provider by default, so a newly
//     discovered deployment of a single-key provider is routable without an
//     operator binding it. The view is the same exact Bind as rule 1, so the
//     attempt, cost and audit records name the key exactly as a binding would.
//  3. Otherwise (no key, or several) the deployment is unroutable. Choosing
//     among several keys is exactly the guess an exact binding exists to
//     prevent.
func (r *Registry) ResolveExecution(deploymentID contract.DeploymentID, slug contract.ProviderSlug, platform bool) (Adapter, *KeyPool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	adapter, ok := r.adapters[slug]
	if !ok {
		if _, realtime := r.sessions[slug]; realtime {
			return nil, nil, fmt.Errorf("provider: %s holds realtime sessions and executes no request", slug)
		}
		return nil, nil, fmt.Errorf("provider: no adapter for %s", slug)
	}
	if !platform {
		return adapter, nil, nil
	}
	pool, err := r.platformCredential(deploymentID, slug, adapter)
	if err != nil {
		return nil, nil, err
	}
	return adapter, pool, nil
}

// ResolveRealtimeExecution is ResolveExecution for a realtime session: the
// session adapter serving the slug and the deployment's exact platform
// credential view, by the same three rules. A slug served by a request adapter
// is refused here, so a text adapter is never handed a session.
func (r *Registry) ResolveRealtimeExecution(deploymentID contract.DeploymentID, slug contract.ProviderSlug) (RealtimeAdapter, *KeyPool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	session, ok := r.sessions[slug]
	if !ok {
		if _, oneShot := r.adapters[slug]; oneShot {
			return nil, nil, fmt.Errorf("provider: %s executes requests: %w", slug, ErrNotASessionAdapter)
		}
		return nil, nil, fmt.Errorf("provider: no realtime adapter for %s", slug)
	}
	pool, err := r.platformCredential(deploymentID, slug, session)
	if err != nil {
		return nil, nil, err
	}
	return session, pool, nil
}

// ResolveCredential answers the credential half of either resolution for any
// served slug. The startup and reload binding gates ask it, so "unbound" there
// and "refused" at execution cannot drift apart.
func (r *Registry) ResolveCredential(deploymentID contract.DeploymentID, slug contract.ProviderSlug) (*KeyPool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	registrant, ok := r.registrant(slug)
	if !ok {
		return nil, fmt.Errorf("provider: no adapter for %s", slug)
	}
	return r.platformCredential(deploymentID, slug, registrant)
}

// platformCredential applies the three rules. The caller holds the read lock.
func (r *Registry) platformCredential(deploymentID contract.DeploymentID, slug contract.ProviderSlug, registrant Registrant) (*KeyPool, error) {
	if !r.bindingsRequired {
		return nil, nil
	}
	source, ok := registrant.(PlatformCredentialSource)
	if !ok {
		return nil, fmt.Errorf("provider: adapter for %s has no platform credential source", slug)
	}
	keyID := ""
	if binding, bound := r.bindings[deploymentID]; bound {
		if binding.Provider != slug {
			return nil, fmt.Errorf("provider: deployment %q has no exact credential binding for %s", deploymentID, slug)
		}
		keyID = binding.KeyID
	} else {
		sole, count := source.PlatformCredentials().SoleKeyID()
		if count != 1 {
			return nil, fmt.Errorf("provider: deployment %q has no exact credential binding for %s, and %s holds %d platform keys, so none is its default", deploymentID, slug, slug, count)
		}
		keyID = sole
	}
	return source.PlatformCredentials().Bind(keyID)
}

// ReplaceGeneration atomically swaps adapters and exact credential bindings.
func (r *Registry) ReplaceGeneration(bindings []CredentialBinding, registrants ...Registrant) error {
	replacement, err := NewRegistry(registrants...)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		if binding.DeploymentID == "" || !binding.Provider.Valid() || binding.KeyID == "" {
			return errors.New("provider: an exact deployment credential binding is invalid")
		}
		if _, duplicate := replacement.bindings[binding.DeploymentID]; duplicate {
			return fmt.Errorf("provider: deployment %q has duplicate credential bindings", binding.DeploymentID)
		}
		registrant, ok := replacement.registrant(binding.Provider)
		if !ok {
			return fmt.Errorf("provider: binding for %q names unconfigured provider %s", binding.DeploymentID, binding.Provider)
		}
		source, ok := registrant.(PlatformCredentialSource)
		if !ok {
			return fmt.Errorf("provider: adapter for %s has no platform credential source", binding.Provider)
		}
		if _, err := source.PlatformCredentials().Bind(binding.KeyID); err != nil {
			return err
		}
		replacement.bindings[binding.DeploymentID] = binding
	}
	replacement.bindingsRequired = true
	r.mu.Lock()
	r.adapters, r.sessions, r.bindings, r.bindingsRequired = replacement.adapters, replacement.sessions, replacement.bindings, replacement.bindingsRequired
	r.mu.Unlock()
	return nil
}

// Serves reports whether an adapter of either kind serves the slug.
func (r *Registry) Serves(slug contract.ProviderSlug) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.serves(slug)
}

// Lookup returns the adapter serving a provider slug.
func (r *Registry) Lookup(slug contract.ProviderSlug) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	adapter, found := r.adapters[slug]
	return adapter, found
}

// All returns every registered adapter of either kind, for the health surface.
func (r *Registry) All() []Registrant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	all := make([]Registrant, 0, len(r.adapters)+len(r.sessions))
	for _, adapter := range r.adapters {
		all = append(all, adapter)
	}
	for _, session := range r.sessions {
		all = append(all, session)
	}
	return all
}

// Replace atomically swaps every adapter after validating the complete new
// registry. Requests already holding an old adapter finish on it; subsequent
// lookups see the new credential pools as one coherent generation.
func (r *Registry) Replace(registrants ...Registrant) error {
	replacement, err := NewRegistry(registrants...)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.adapters, r.sessions = replacement.adapters, replacement.sessions
	r.mu.Unlock()
	return nil
}

// Executes reports whether an adapter declared a request family. The executor
// asks before Translate, so an adapter's refusal logic is a second line rather
// than the only thing keeping a request off an endpoint that cannot serve it.
func Executes(adapter Adapter, format contract.APIFormat) bool {
	for _, declared := range adapter.APIFormats() {
		if declared == format {
			return true
		}
	}
	return false
}

// ChatOutputDeclarer is a request adapter that states, for one deployment,
// what its chat_completions path produces: written text, spoken output, or
// both. The answer is per deployment because it depends on the model as well
// as on the adapter (providerconfig.SpeaksAloud).
type ChatOutputDeclarer interface {
	ChatOutputs(route Route) providerconfig.ChatOutput
}

// ChatOutputsOf is what an adapter's chat_completions path produces on one
// deployment. An adapter that declares nothing writes text and never speaks.
// The executor asks before Translate, as it asks Executes, so a spoken request
// never reaches an adapter or a model that cannot answer aloud and a text chat
// never reaches one that only speaks.
func ChatOutputsOf(adapter Adapter, route Route) providerconfig.ChatOutput {
	if declarer, ok := adapter.(ChatOutputDeclarer); ok {
		return declarer.ChatOutputs(route)
	}
	return providerconfig.ChatOutput{Text: true}
}

// MaxAudioChunkBytes is the most raw audio one contract audio event carries.
const MaxAudioChunkBytes = 49152

// EmitAudio sends raw audio as bounded audio events, stopping between chunks
// once ctx is done.
func EmitAudio(ctx context.Context, out AudioEmitter, outputIndex int, mediaType string, data []byte) error {
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		size := min(len(data), MaxAudioChunkBytes)
		if err := out.Audio(outputIndex, mediaType, data[:size]); err != nil {
			return err
		}
		data = data[size:]
	}
	return nil
}

// AudioEmitter extends semantic output for providers producing binary audio.
// The executor owns framing and ordering, just as it does for text deltas.
type AudioEmitter interface {
	Emitter
	Audio(outputIndex int, mediaType string, data []byte) error
}
