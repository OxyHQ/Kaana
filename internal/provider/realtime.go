package provider

import (
	"context"
	"errors"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// A realtime session is not a request, so it is not an Adapter.
//
// Adapter's shape is one call, one stream and one terminal outcome: Translate
// builds the call, Stream reads it to the end. A session is a conversation the
// client keeps steering for up to an hour — commands in, events out, in no
// fixed ratio — and its conversation state lives upstream, so it can neither
// be retried nor failed over once it has opened. Folding it into Adapter would
// make Translate and Stream mean two things, so it is its own interface, and a
// slug resolves to exactly one of the two (Registry refuses a type that is
// both). A request adapter is therefore never handed a session, and a session
// adapter never a request.
//
// What a session adapter does NOT do is the same list Adapter's documentation
// gives: it allocates no id, numbers no event, decides no terminality beyond
// reporting that its upstream ended, and never decides what a failure means for
// the KEY — opening a session walks the credential view through
// WalkAttempts, the same rules Walk applies to a request.

// RealtimeAdapter opens upstream realtime sessions for one provider slug.
type RealtimeAdapter interface {
	Provider() contract.ProviderSlug
	// RealtimeSessionKinds declares the session kinds this adapter can open at
	// all, from providerconfig.RealtimeSessionKinds. The registry refuses an
	// adapter that declares none; Open still refuses a kind the deployment's
	// own model cannot hold.
	RealtimeSessionKinds() []contract.RealtimeSessionKind
	// Open opens one session on the exact credential view it is given. A
	// session the provider cannot express is refused with ErrUnsupported before
	// anything is dialled. The returned RealtimeOpened names the key the
	// attempt spent even when Open fails, because the operator cost record needs
	// it either way.
	Open(ctx context.Context, request RealtimeOpenRequest, credentials *KeyPool) (RealtimeUpstream, RealtimeOpened, error)
	Health(ctx context.Context) Health
}

// RealtimeOpenRequest is what a session adapter is asked to open.
type RealtimeOpenRequest struct {
	// RequestID identifies credential-attempt telemetry and the errors the
	// upstream reports; it carries no credential.
	RequestID contract.RequestID
	Route     Route
	// CredentialAttempts is the session request's credential attempt sequence,
	// shared by every open attempt it makes (see Call.CredentialAttempts).
	CredentialAttempts *CredentialAttemptSequence
	Kind               contract.RealtimeSessionKind
	Config             contract.RealtimeSessionConfig
}

// RealtimeOpened is what one open attempt spent.
type RealtimeOpened struct {
	KeyID    string
	KeyClass KeyClass
}

// RealtimeUpstream is one open upstream session.
//
// Send and Next are called from different goroutines: the session applies
// commands while a pump reads events. Close may be called from either, and more
// than once.
//
// A `live` session (Kaana ADR 0012) is served by an adapter that lists
// contract.RealtimeLive in RealtimeSessionKinds, natively or by emulation, on
// this same interface. Next hands a turn over by returning a
// *contract.RealtimeDelegationRequestedEvent: a delegation id unique in the
// session, and the caller's words since the previous delegation. Send then
// receives *contract.RealtimeDelegationReplyCommand,
// *contract.RealtimeDelegationContextCommand and
// *contract.RealtimeDelegationCancelCommand, only ever for the newest
// delegation and only until its final reply or its cancel, and an upstream
// that cannot express one returns ErrUnsupported like any other command. The
// session owns the rest, so every adapter shares it: it numbers the events,
// emits delegation.superseded when a newer delegation opens before the open
// one's final reply (Next never returns one), refuses a command naming any
// other delegation, and refuses session.update, input_audio.commit and
// input_audio.clear before Send sees them.
type RealtimeUpstream interface {
	// Send applies one decoded command upstream, once. It never retries: a
	// command whose write failed is gone, because resending it could apply it
	// twice. A command the provider cannot express is refused with
	// ErrUnsupported before anything is written, and the session continues.
	Send(ctx context.Context, command contract.RealtimeCommand) error
	// Next returns the next normalized event. It returns
	// ErrRealtimeUpstreamClosed when the provider ended the session cleanly and
	// a classified error (ErrUpstream) when it failed.
	Next(ctx context.Context) (RealtimeUpstreamEvent, error)
	// Close ends the upstream session.
	Close() error
}

// RealtimeUpstreamEvent is one normalized event, or one measurement, or both.
type RealtimeUpstreamEvent struct {
	// Event is unstamped: the session numbers it. Nil when the upstream
	// reported a measurement with no contract event of its own.
	Event contract.RealtimeServerEvent
	// CommandID names the command this event answers, when the provider said
	// which one it answers.
	CommandID *contract.RealtimeCommandID
	// Units are what the provider reported this event consumed. The session
	// sums them into its totals; each unit appears at most once here.
	Units []contract.UsageQuantity
}

// RealtimeMeter is an open session whose provider bills what Kaana itself
// measures — audio it sent and received, events it wrote — rather than units
// the provider reports (xAI's Voice Agent API bills audio by the minute and
// text inputs per event, and reports only tokens it does not bill). Measured
// is the session's cumulative measurement; the session reads it once, when it
// settles, adds it to its totals and reports them `oxy_measured`. It must be
// safe to call while Send and Next run.
type RealtimeMeter interface {
	Measured() []contract.UsageQuantity
}

// RealtimeFinalUsage is an open session whose provider reports the session's
// whole usage once, when it ends: OpenAI GPT-Live's `session.closed` carries
// the session's billed seconds, and its earlier `session.usage.updated` are
// cumulative snapshots, not increments. The session calls FinalUsage once, when
// it settles and before Close: the adapter ends its side of the provider's
// session, waiting a bounded time for that report, and returns the units with
// their source — contract.UsageProviderReported when the provider confirmed
// them, contract.UsageOxyMeasured when it never did (the connection ended
// first) and the units are Kaana's own measurement. Such a session's events
// carry no units, so nothing is counted twice. ctx carries no cancellation the
// settlement must obey; the adapter bounds the wait itself.
type RealtimeFinalUsage interface {
	FinalUsage(ctx context.Context) RealtimeFinal
}

// RealtimeFinal is how a RealtimeFinalUsage session ended.
type RealtimeFinal struct {
	Units  []contract.UsageQuantity
	Source contract.UsageSource
	// Trailing are the events that end what the session was carrying when it
	// settled, which only the provider's end produces: the transcript.done of
	// an item still open (GPT-Live has no event that ends an utterance). The
	// session emits them, unstamped and in order, before session.closed; Next
	// never returns them.
	Trailing []contract.RealtimeServerEvent
}

// ErrRealtimeUpstreamClosed reports that the provider ended a session without
// reporting a failure.
var ErrRealtimeUpstreamClosed = errors.New("provider: the upstream ended the realtime session")

// ErrNotASessionAdapter reports that a deployment's slug is served by a
// request adapter, so no realtime session can be opened on it. It is a fact
// about the route, not about the provider's health.
var ErrNotASessionAdapter = errors.New("provider: the deployment's adapter holds no realtime session")

// OpensRealtime reports whether an adapter declared a session kind.
func OpensRealtime(adapter RealtimeAdapter, kind contract.RealtimeSessionKind) bool {
	for _, declared := range adapter.RealtimeSessionKinds() {
		if declared == kind {
			return true
		}
	}
	return false
}
