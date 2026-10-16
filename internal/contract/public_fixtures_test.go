package contract

import (
	"testing"
)

// publicValidFixtures are the public shapes Kaana writes to a client, each
// built the way internal/publicapi builds it (the projections included), and
// parsed by the contract's own Zod schema.
func publicValidFixtures(t *testing.T) []fixture {
	t.Helper()
	attribution := sampleAttribution()
	session := attribution.RequestID
	at := Timestamp("2026-10-15T10:00:00.000Z")

	// An internal failure with a provider passthrough and an internal-only
	// code: the projection drops the provider and retires the code.
	category := UpstreamOverloaded
	internal := NewError(session, CodeDeploymentUnavailable, "all 2 authorized deployments are out of rotation (groq, cerebras)").WithRetryAfter(1500)
	internal.UpstreamCategory = &category
	internal.ProviderError = &ProviderErrorPassthrough{Provider: "groq"}
	unavailable := PublicErrorFrom(internal)
	if unavailable.Code != "service_unavailable" || !unavailable.Retryable || unavailable.RetryAfterMs == nil {
		t.Fatalf("an internal-only failure projected to %+v", unavailable)
	}
	invalid := PublicErrorFrom(NewError(session, CodeInvalidRequest, "ElevenLabs refuses speed 1.5").WithParam("speed"))
	if invalid.Param == nil || *invalid.Param != "speed" {
		t.Fatalf("the param was lost: %+v", invalid)
	}

	created := PublicRealtimeEvent(&RealtimeSessionCreatedEvent{
		ResolvedModelReference: "openai/gpt-realtime@2026-08-28", ServingProvider: "openai", DeploymentID: "dep_openai_realtime",
		Kind: RealtimeConversation, Config: RealtimeSessionConfig{
			InputAudioFormat: RealtimePCM16, TurnDetection: RealtimeTurnDetection{Type: TurnDetectionNone},
			OutputModalities: []RealtimeOutputModality{RealtimeOutputText},
		},
		Limits:         RealtimeSessionLimits{MaxDurationMs: 60_000, IdleTimeoutMs: 30_000, MaxInputAudioBytes: 1_000, MaxOutputAudioBytes: 1_000, MaxResponses: 10},
		ResumeWindowMs: 30_000, StartedAt: at, ExpiresAt: "2026-10-15T10:01:00.000Z",
	})
	created.Stamp(RealtimeEventHeader{SchemaVersion: 1, RequestID: session, Sequence: 0})
	done := PublicRealtimeEvent(&RealtimeResponseDoneEvent{ResponseID: "resp_1", Status: RealtimeResponseCompleted,
		FinishReason: pointerTo(FinishStop), DeploymentID: "dep_openai_realtime",
		Units: []UsageQuantity{{Unit: UnitOutputTokens, Quantity: 4}}, UsageSource: UsageProviderReported})
	done.Stamp(RealtimeEventHeader{SchemaVersion: 1, RequestID: session, Sequence: 7, CommandID: pointerTo(RealtimeCommandID("cmd-respond"))})
	failure := PublicRealtimeEvent(&RealtimeErrorEvent{Fatal: true, Error: *internal})
	failure.Stamp(RealtimeEventHeader{SchemaVersion: 1, RequestID: session, Sequence: 8})
	closed := PublicRealtimeEvent(&RealtimeSessionClosedEvent{Reason: RealtimeClosedByClient, DeploymentID: pointerTo(DeploymentID("dep_openai_realtime")),
		Units: []UsageQuantity{{Unit: UnitOutputTokens, Quantity: 4}}, UsageSource: UsageProviderReported, ClosedAt: at})
	closed.Stamp(RealtimeEventHeader{SchemaVersion: 1, RequestID: session, Sequence: 9})
	unopened := PublicRealtimeEvent(&RealtimeSessionClosedEvent{Reason: RealtimeNoRouteAvailable, UsageSource: UsageProviderReported, ClosedAt: at})
	unopened.Stamp(RealtimeEventHeader{SchemaVersion: 1, RequestID: session, Sequence: 1})

	language := LanguageTag("es-ES")
	format, sse := SpeechFormat("mp3"), SpeechStreamSSE
	return []fixture{
		{Schema: "publicErrorSchema", Case: "projected-service-unavailable", Value: unavailable},
		{Schema: "publicErrorSchema", Case: "projected-invalid-request", Value: invalid},
		{Schema: "realtimePublicServerEventSchema", Case: "session-created", Value: created},
		{Schema: "realtimePublicServerEventSchema", Case: "response-done", Value: done},
		{Schema: "realtimePublicServerEventSchema", Case: "fatal-error", Value: failure},
		{Schema: "realtimePublicServerEventSchema", Case: "session-closed", Value: closed},
		{Schema: "realtimePublicServerEventSchema", Case: "closed-before-open", Value: unopened},
		{Schema: "speechRequestSchema", Case: "streamed-sse", Value: SpeechRequest{
			Model: "elevenlabs/eleven-multilingual", Input: "Hola", Voice: "rachel", ResponseFormat: &format,
			Speed: pointerTo(1.1), Stream: pointerTo(true), StreamFormat: &sse, User: pointerTo("user_1"),
		}},
		{Schema: "speechStreamEventSchema", Case: "delta", Value: SpeechAudioDeltaEvent{Type: "speech.audio.delta", Audio: "SUQzBAAAAAAA"}},
		{Schema: "speechStreamEventSchema", Case: "done", Value: SpeechAudioDoneEvent{Type: "speech.audio.done", RequestID: session,
			Model: "elevenlabs/eleven-multilingual@v2", Usage: []UsageQuantity{{Unit: UnitCharacters, Quantity: 4}}}},
		{Schema: "speechStreamEventSchema", Case: "error", Value: SpeechStreamErrorEvent{Type: "error", Error: *unavailable}},
		{Schema: "transcriptionSchema", Case: "detected-language", Value: Transcription{SchemaVersion: 1, RequestID: session,
			Model: "openai/whisper-1@2023-03-01", Text: "Hola mundo", Language: &language, DurationMs: pointerTo(1200),
			Usage: []UsageQuantity{{Unit: UnitAudioInputMilliseconds, Quantity: 1200}}}},
		{Schema: "transcriptionJsonRequestSchema", Case: "every-field", Value: TranscriptionJSONRequest{Model: "openai/whisper-1",
			Language: &language, Prompt: pointerTo("Kaana"),
			Audio: TranscriptionAudio{MediaType: "audio/wav", Data: "UklGRiQAAABXQVZF"}}},
		{Schema: "responseStreamEventSchema", Case: "start", Value: ResponseStreamStartEvent{SchemaVersion: 1, Type: EventStart,
			RequestID: session, Seq: 0, GenerationID: pointerTo(GenerationID("gen_1")), Model: "openai/gpt-5@2026-05-01", StartedAt: at}},
		{Schema: "responseStreamEventSchema", Case: "usage", Value: ResponseStreamUsageEvent{SchemaVersion: 1, Type: EventUsage,
			RequestID: session, Seq: 2, Units: []UsageQuantity{{Unit: UnitOutputTokens, Quantity: 4}}, UsageSource: UsageProviderReported}},
		{Schema: "responseStreamEventSchema", Case: "error", Value: ResponseStreamErrorEvent{SchemaVersion: 1, Type: EventError,
			RequestID: session, Seq: 3, Error: *unavailable}},
	}
}

// publicInvalidFixtures are what the public contract must refuse; each is a
// leak or a rule the projections exist to prevent.
func publicInvalidFixtures(t *testing.T) []fixture {
	t.Helper()
	session := sampleAttribution().RequestID
	leaked := NewPublicError(session, "invalid_request", "bad")
	return []fixture{
		// An internal-only code that escaped the projection.
		{Schema: "publicErrorSchema", Case: "internal-only-code", Value: PublicError{SchemaVersion: 1, Code: "deployment_unavailable",
			Message: "no route", Retryable: true, RequestID: session}},
		// A retry hint on a code that can never succeed on retry.
		{Schema: "publicErrorSchema", Case: "retry-hint-on-final-code", Value: PublicError{SchemaVersion: 1, Code: leaked.Code,
			Message: leaked.Message, Retryable: false, RequestID: session, RetryAfterMs: pointerTo(10)}},
	}
}
