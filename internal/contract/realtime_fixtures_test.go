package contract

import (
	"encoding/json"
	"reflect"
	"testing"
)

// realtimeValidFixtures covers contract set 3.2.0: spoken output from a
// conversational model, the audio-token units, and one fixture for every
// realtime command and event, each parsed by the published union that carries
// it rather than only by its own variant.
func realtimeValidFixtures(t *testing.T) []fixture {
	t.Helper()
	attribution := sampleAttribution()
	session := attribution.RequestID
	started := Timestamp("2026-09-30T09:41:00.000Z")
	frame := "AAAAAAAA"

	audioChat := Request{
		SchemaVersion: RequestEnvelopeVersion,
		Attribution:   attribution,
		Target:        RoutingTarget{Kind: TargetModel, ModelReference: pointerTo(ModelReference("openai/gpt-audio-1.5@2026-09-01"))},
		Modality:      ModalityAudio,
		Input: Input{Format: InputMessages, Messages: []Message{{
			Role:    RoleUser,
			Content: []ContentPart{{Type: ContentPartText, Text: pointerTo("say hello")}},
		}}},
		Stream:      true,
		AudioOutput: &AudioOutputParameters{Voice: "alloy", Format: AudioOutputPCM},
		Client:      ClientRequestMetadata{APIFormat: APIFormatChatCompletions, Endpoint: "/v1/chat/completions", ReceivedAt: started},
		RoutingPolicy: RoutingPolicyReference{
			RoutingPolicyID: "rp_01JQZ", PolicyVersion: 3,
		},
		AuthorizedRoutes: []AuthorizedRoute{{
			Substitution: SubstitutionSameModel, DeploymentID: "dep_openai_audio_chat",
			ModelReference: "openai/gpt-audio-1.5@2026-09-01", Provider: "openai-audio", Regions: []Region{},
		}},
	}
	if err := audioChat.Validate(); err != nil {
		t.Fatalf("the audio chat fixture does not satisfy Kaana's own validation: %v", err)
	}
	audioReport := UsageReport{
		SchemaVersion: UsageReportSchemaVersion, RequestID: session, GenerationID: attribution.GenerationID,
		Attribution: attribution, Outcome: OutcomeCompleted,
		Units: []UsageQuantity{
			{Unit: UnitInputTokens, Quantity: 12}, {Unit: UnitAudioInputTokens, Quantity: 90},
			{Unit: UnitCachedAudioInputTokens, Quantity: 10}, {Unit: UnitOutputTokens, Quantity: 8},
			{Unit: UnitAudioOutputTokens, Quantity: 120},
		},
		UsageSource: UsageProviderReported, ResolvedModelReference: "openai/gpt-audio-1.5@2026-09-01",
		ServingProvider: "openai-audio", DeploymentID: "dep_openai_audio_chat", StartedAt: started, CompletedAt: started,
	}
	if err := audioReport.Validate(); err != nil {
		t.Fatalf("the audio token usage report does not satisfy Kaana's own validation: %v", err)
	}

	serverVAD := RealtimeTurnDetection{
		Type: TurnDetectionServerVAD, Threshold: pointerTo(0.5), PrefixPaddingMs: pointerTo(300),
		SilenceDurationMs: pointerTo(500), CreateResponse: pointerTo(true), InterruptResponse: pointerTo(true),
	}
	config := RealtimeSessionConfig{
		Instructions:            pointerTo("be brief"),
		OutputModalities:        []RealtimeOutputModality{RealtimeOutputAudio},
		Voice:                   pointerTo("marin"),
		InputAudioFormat:        RealtimePCM16,
		OutputAudioFormat:       pointerTo(RealtimePCM16),
		TurnDetection:           serverVAD,
		InputAudioTranscription: &RealtimeInputTranscription{Language: pointerTo("en"), Prompt: pointerTo("names")},
		Tools: []ToolDefinition{{
			Type: "function", Name: "lookup", Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		}},
		ToolChoice:      &ToolChoice{Mode: pointerTo(ToolChoiceAuto)},
		Temperature:     pointerTo(0.8),
		MaxOutputTokens: pointerTo(4096),
	}
	limits := RealtimeSessionLimits{
		MaxDurationMs: 600_000, IdleTimeoutMs: 60_000, MaxInputAudioBytes: 28_800_000,
		MaxOutputAudioBytes: 28_800_000, MaxResponses: 200,
	}
	request := RealtimeSessionRequest{
		SchemaVersion: 1, Attribution: attribution, ModelReference: "openai/gpt-realtime-2.1",
		Kind: RealtimeConversation, Transport: RealtimeWebSocket, Config: config, Limits: limits,
		Client: RealtimeClientMetadata{
			Endpoint: "/v1/realtime", ClientSessionID: pointerTo("client-session-1"), ReceivedAt: started,
			Labels: map[string]string{"team": "voice"},
		},
		RoutingPolicy: RoutingPolicyReference{RoutingPolicyID: "rp_01JQZ", PolicyVersion: 3},
		AuthorizedRoutes: []AuthorizedRoute{{
			Substitution: SubstitutionSameModel, DeploymentID: "dep_openai_realtime",
			ModelReference: "openai/gpt-realtime-2.1@2026-09-01", Provider: "openai-realtime", Regions: []Region{},
		}},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("the realtime session fixture does not satisfy Kaana's own validation: %v", err)
	}
	transcription := request
	transcription.Kind = RealtimeTranscription
	transcription.Config = RealtimeSessionConfig{
		InputAudioFormat: RealtimeULaw,
		TurnDetection: RealtimeTurnDetection{
			Type: TurnDetectionSemanticVAD, Eagerness: pointerTo(RealtimeVADEagerness("low")),
			CreateResponse: pointerTo(false), InterruptResponse: pointerTo(false),
		},
		InputAudioTranscription: &RealtimeInputTranscription{},
	}
	if err := transcription.Validate(); err != nil {
		t.Fatalf("the transcription session fixture does not satisfy Kaana's own validation: %v", err)
	}

	base := func(id string) (int, RequestID, RealtimeCommandID) { return 1, session, RealtimeCommandID(id) }
	commands := []RealtimeCommand{}
	add := func(command RealtimeCommand) { commands = append(commands, command) }
	{
		v, r, c := base("cmd-update")
		add(&RealtimeSessionUpdateCommand{SchemaVersion: v, RequestID: r, CommandID: c, Type: "session.update",
			Config: RealtimeSessionConfigUpdate{Instructions: pointerTo("be terse"), TurnDetection: &RealtimeTurnDetection{Type: TurnDetectionNone}}})
	}
	{
		v, r, c := base("cmd-item")
		add(&RealtimeItemCreateCommand{SchemaVersion: v, RequestID: r, CommandID: c, Type: "conversation.item.create",
			PreviousItemID: pointerTo(RealtimeItemID("item_0")),
			Item: RealtimeConversationItem{Type: RealtimeMessageItem, Role: pointerTo(RealtimeItemRole("user")), Content: []RealtimeContentPart{
				{Type: RealtimeInputTextPart, Text: pointerTo("hello")},
				{Type: RealtimeInputAudioPart, Format: pointerTo(RealtimePCM16), Data: pointerTo(frame)},
			}}})
	}
	{
		v, r, c := base("cmd-delete")
		add(&RealtimeItemDeleteCommand{SchemaVersion: v, RequestID: r, CommandID: c, Type: "conversation.item.delete", ItemID: "item_1"})
	}
	{
		v, r, c := base("cmd-truncate")
		add(&RealtimeItemTruncateCommand{SchemaVersion: v, RequestID: r, CommandID: c, Type: "conversation.item.truncate", ItemID: "item_2", AudioEndMs: 1500})
	}
	{
		v, r, c := base("cmd-append")
		add(&RealtimeInputAudioAppendCommand{SchemaVersion: v, RequestID: r, CommandID: c, Type: "input_audio.append", Data: frame})
	}
	{
		v, r, c := base("cmd-commit")
		add(&RealtimeInputAudioCommitCommand{SchemaVersion: v, RequestID: r, CommandID: c, Type: "input_audio.commit"})
	}
	{
		v, r, c := base("cmd-clear")
		add(&RealtimeInputAudioClearCommand{SchemaVersion: v, RequestID: r, CommandID: c, Type: "input_audio.clear"})
	}
	{
		v, r, c := base("cmd-respond")
		add(&RealtimeResponseCreateCommand{SchemaVersion: v, RequestID: r, CommandID: c, Type: "response.create",
			Response: &RealtimeResponseParameters{Instructions: pointerTo("answer"), OutputModalities: []RealtimeOutputModality{RealtimeOutputText},
				MaxOutputTokens: pointerTo(100), ToolChoice: &ToolChoice{Mode: pointerTo(ToolChoiceNone)}}})
	}
	{
		v, r, c := base("cmd-cancel")
		add(&RealtimeResponseCancelCommand{SchemaVersion: v, RequestID: r, CommandID: c, Type: "response.cancel", ResponseID: pointerTo(RealtimeResponseID("resp_1"))})
	}
	{
		v, r, c := base("cmd-resume")
		add(&RealtimeSessionResumeCommand{SchemaVersion: v, RequestID: r, CommandID: c, Type: "session.resume", AfterSequence: -1})
	}
	{
		v, r, c := base("cmd-close")
		add(&RealtimeSessionCloseCommand{SchemaVersion: v, RequestID: r, CommandID: c, Type: "session.close"})
	}

	answering := pointerTo(RealtimeCommandID("cmd-respond"))
	item := RealtimeConversationItem{Type: RealtimeMessageItem, ItemID: pointerTo(RealtimeItemID("item_3")),
		Role: pointerTo(RealtimeItemRole("assistant")), Content: []RealtimeContentPart{
			{Type: RealtimeOutputTextPart, Text: pointerTo("hi")},
			{Type: RealtimeOutputAudioPart, Format: pointerTo(RealtimePCM16), Transcript: pointerTo("hi")},
		}}
	call := RealtimeConversationItem{Type: RealtimeFunctionCallItem, ItemID: pointerTo(RealtimeItemID("item_4")),
		CallID: pointerTo("call_1"), Name: pointerTo("lookup"), Arguments: pointerTo(`{"q":"x"}`)}
	events := []RealtimeServerEvent{
		&RealtimeSessionCreatedEvent{ResolvedModelReference: "openai/gpt-realtime-2.1@2026-09-01", ServingProvider: "openai-realtime",
			DeploymentID: "dep_openai_realtime", Kind: RealtimeConversation, Config: config, Limits: limits,
			ResumeWindowMs: 30_000, StartedAt: started, ExpiresAt: "2026-09-30T09:51:00.000Z"},
		&RealtimeSessionUpdatedEvent{Config: config},
		&RealtimeSessionResumedEvent{AfterSequence: 4},
		&RealtimeCommandAcceptedEvent{Duplicate: true},
		&RealtimeItemAddedEvent{ItemID: "item_3", PreviousItemID: pointerTo(RealtimeItemID("item_2")), Item: item},
		&RealtimeItemDoneEvent{ItemID: "item_4", Item: call},
		&RealtimeItemDeletedEvent{ItemID: "item_1"},
		&RealtimeItemTruncatedEvent{ItemID: "item_3", ContentIndex: 1, AudioEndMs: 1500},
		&RealtimeSpeechStartedEvent{ItemID: "item_5", AudioStartMs: 120},
		&RealtimeSpeechStoppedEvent{ItemID: "item_5", AudioEndMs: 2400},
		&RealtimeInputAudioCommittedEvent{ItemID: "item_5", PreviousItemID: pointerTo(RealtimeItemID("item_4"))},
		&RealtimeInputAudioClearedEvent{},
		&RealtimeResponseCreatedEvent{ResponseID: "resp_1"},
		&RealtimeOutputAudioDeltaEvent{ResponseID: "resp_1", ItemID: "item_3", ContentIndex: 1, Format: RealtimePCM16, Data: frame},
		&RealtimeOutputAudioDoneEvent{ResponseID: "resp_1", ItemID: "item_3", ContentIndex: 1},
		&RealtimeTranscriptDeltaEvent{Source: RealtimeTranscriptOutput, ItemID: "item_3", ContentIndex: 1, ResponseID: pointerTo(RealtimeResponseID("resp_1")), Text: "h"},
		&RealtimeTranscriptDoneEvent{Source: RealtimeTranscriptInput, ItemID: "item_5", Transcript: "hello there"},
		&RealtimeTextDeltaEvent{ResponseID: "resp_1", ItemID: "item_3", Text: "hi"},
		&RealtimeToolCallEvent{ResponseID: "resp_1", ItemID: "item_4", ToolCallID: "call_1", Name: pointerTo("lookup"), ArgumentsDelta: pointerTo(`{"q":`), Complete: false},
		&RealtimeResponseDoneEvent{ResponseID: "resp_1", Status: RealtimeResponseCompleted, FinishReason: pointerTo(FinishStop),
			DeploymentID: "dep_openai_realtime", Units: []UsageQuantity{{Unit: UnitAudioInputTokens, Quantity: 40}, {Unit: UnitAudioOutputTokens, Quantity: 80}},
			UsageSource: UsageProviderReported},
		&RealtimeErrorEvent{Fatal: false, Error: *NewError(session, CodeInvalidRequest, "the command names no item")},
		&RealtimeSessionClosedEvent{Reason: RealtimeClosedByClient, DeploymentID: pointerTo(DeploymentID("dep_openai_realtime")),
			Units: []UsageQuantity{{Unit: UnitAudioInputTokens, Quantity: 40}}, UsageSource: UsageProviderReported, ClosedAt: started},
	}
	for index, event := range events {
		event.Stamp(RealtimeEventHeader{SchemaVersion: 1, RequestID: session, Sequence: index, CommandID: answering})
	}

	fixtures := []fixture{
		{Schema: "inferenceRequestSchema", Case: "audio-chat-streamed-pcm", Value: audioChat},
		{Schema: "normalizedUsageReportSchema", Case: "audio-token-units", Value: audioReport},
		{Schema: "inferenceStreamEventSchema", Case: "delta-output-audio-transcript", Value: &StreamDeltaEvent{
			SchemaVersion: SchemaVersion, Type: EventDelta, RequestID: session, Seq: 2, OutputIndex: 0,
			Channel: ChannelOutputAudioTranscript, Text: "hello",
		}},
		{Schema: "realtimeSessionRequestSchema", Case: "conversation-with-every-optional-field", Value: request},
		{Schema: "realtimeSessionRequestSchema", Case: "transcription-semantic-vad", Value: transcription},
	}
	for _, command := range commands {
		encoded, err := json.Marshal(command)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeRealtimeCommand(encoded, session); err != nil {
			t.Fatalf("Kaana cannot decode its own %s fixture: %v", command.CommandType(), err)
		}
		fixtures = append(fixtures, fixture{Schema: "realtimeClientCommandSchema", Case: string(command.CommandType()), Value: command})
	}
	for _, event := range events {
		fixtures = append(fixtures, fixture{Schema: "realtimeServerEventSchema", Case: string(event.EventType()), Value: event})
	}
	return fixtures
}

// realtimeInvalidFixtures are shapes Kaana's types can express and the
// published schemas must refuse.
func realtimeInvalidFixtures(t *testing.T) []fixture {
	t.Helper()
	valid := realtimeValidFixtures(t)
	request := valid[3].Value.(RealtimeSessionRequest)

	substituted := request
	substituted.AuthorizedRoutes = []AuthorizedRoute{{
		Substitution: SubstitutionCrossModel, DeploymentID: "dep_other",
		ModelReference: "openai/gpt-5@2026-05-01", Provider: "openai",
	}}
	transcriptionThatResponds := valid[4].Value.(RealtimeSessionRequest)
	transcriptionThatResponds.Config.Voice = pointerTo("marin")

	audioChat := valid[0].Value.(Request)
	audioChat.AudioOutput = &AudioOutputParameters{Voice: "alloy", Format: "mp3"}

	return []fixture{
		{Schema: "realtimeSessionRequestSchema", Case: "cross-model-session", Value: substituted},
		{Schema: "realtimeSessionRequestSchema", Case: "transcription-with-a-voice", Value: transcriptionThatResponds},
		{Schema: "inferenceRequestSchema", Case: "streamed-audio-output-not-pcm", Value: audioChat},
		{Schema: "realtimeServerEventSchema", Case: "response-done-unit-twice", Value: &RealtimeResponseDoneEvent{
			SchemaVersion: 1, RequestID: "req_01JQZABCDEF", Sequence: 1, Type: "response.done", ResponseID: "resp_1",
			Status: RealtimeResponseCompleted, DeploymentID: "dep_openai_realtime",
			Units:       []UsageQuantity{{Unit: UnitAudioInputTokens, Quantity: 1}, {Unit: UnitAudioInputTokens, Quantity: 2}},
			UsageSource: UsageProviderReported,
		}},
		{Schema: "realtimeClientCommandSchema", Case: "audio-frame-not-base64", Value: &RealtimeInputAudioAppendCommand{
			SchemaVersion: 1, RequestID: "req_01JQZABCDEF", CommandID: "cmd", Type: "input_audio.append", Data: "not base64!",
		}},
	}
}

func TestRealtimeFixturesAreRefusedByKaanaToo(t *testing.T) {
	for _, item := range realtimeInvalidFixtures(t) {
		switch value := item.Value.(type) {
		case RealtimeSessionRequest:
			if err := value.Validate(); err == nil {
				t.Errorf("%s: Kaana accepts a session the contract refuses", item.Case)
			}
		case Request:
			if err := value.Validate(); err == nil {
				t.Errorf("%s: Kaana accepts a request the contract refuses", item.Case)
			}
		case RealtimeCommand:
			encoded, _ := json.Marshal(value)
			if _, err := DecodeRealtimeCommand(encoded, "req_01JQZABCDEF"); err == nil {
				t.Errorf("%s: Kaana accepts a command the contract refuses", item.Case)
			}
		}
	}
	if _, err := DecodeRealtimeCommand([]byte(`{"schemaVersion":1,"requestId":"req_other","commandId":"c","type":"input_audio.commit"}`), "req_01JQZABCDEF"); err == nil {
		t.Error("a command naming another session was accepted")
	}
	if _, err := DecodeRealtimeCommand([]byte(`{"schemaVersion":1,"requestId":"req_01JQZABCDEF","commandId":"c","type":"input_audio.commit","extra":1}`), "req_01JQZABCDEF"); err == nil {
		t.Error("a command with an unknown field was accepted")
	}
}

func TestDecodedRealtimeAudioBytesCountsTheAudioNotTheText(t *testing.T) {
	for encoded, want := range map[string]int{"AAAAAAAA": 6, "AAAAAA==": 4, "AAAAAAA=": 5} {
		if got := DecodedRealtimeAudioBytes(encoded); got != want {
			t.Errorf("DecodedRealtimeAudioBytes(%q) = %d, want %d", encoded, got, want)
		}
	}
}

// TestRealtimeDiscriminatorsMatchThePublishedUnions pins the Go command and
// event type vocabularies to the published unions, and each Go type to the
// discriminator it reports.
func TestRealtimeDiscriminatorsMatchThePublishedUnions(t *testing.T) {
	file := loadDescriptor(t)
	for union, goValues := range map[string][]string{
		"realtimeClientCommandSchema": stringsOf(realtimeCommandTypeValues),
		"realtimeServerEventSchema":   stringsOf(realtimeEventTypeValues),
	} {
		published := make([]string, 0)
		for _, variant := range file.Shapes[union].Variants {
			for _, field := range file.Shapes[variant.Ref].Fields {
				if field.Name == "type" {
					var literal string
					if err := json.Unmarshal(field.Value, &literal); err != nil {
						t.Fatal(err)
					}
					published = append(published, literal)
				}
			}
		}
		if diff := diffStringLists(published, goValues); diff != "" {
			t.Errorf("%s differs from Go:\n%s", union, diff)
		}
		for literal, goType := range goUnionOfNamedShapes[union] {
			instance := reflect.New(goType).Interface()
			switch value := instance.(type) {
			case RealtimeCommand:
				if string(value.CommandType()) != literal {
					t.Errorf("%s reports %q, registered as %q", goType, value.CommandType(), literal)
				}
			case RealtimeServerEvent:
				if string(value.EventType()) != literal {
					t.Errorf("%s reports %q, registered as %q", goType, value.EventType(), literal)
				}
			default:
				t.Errorf("%s is neither a command nor an event", goType)
			}
		}
	}
}
