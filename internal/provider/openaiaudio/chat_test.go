package openaiaudio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// spoken records what an adapter emitted, in order, as the executor's emitter
// would receive it. It enforces the audio event bounds the executor enforces,
// so a chunk the executor would refuse fails here too.
type spoken struct {
	started   bool
	channels  map[contract.DeltaChannel]string
	audio     []byte
	chunks    int
	mediaType string
	units     []contract.UsageQuantity
	failAfter int // fail the Nth audio chunk; 0 never fails
	order     []string
}

var errSinkClosed = errors.New("sink closed")

func (s *spoken) Start(contract.ModelReference, time.Time) error {
	if s.started {
		return errors.New("second start")
	}
	s.started, s.channels = true, map[contract.DeltaChannel]string{}
	s.order = append(s.order, "start")
	return nil
}
func (s *spoken) Delta(index int, channel contract.DeltaChannel, text string) error {
	if !s.started || index != 0 {
		return errors.New("delta before start or on a second output")
	}
	s.channels[channel] += text
	s.order = append(s.order, "delta:"+string(channel))
	return nil
}
func (s *spoken) ToolCall(provider.ToolCallDelta) error { return errors.New("unexpected tool call") }
func (s *spoken) Usage(u []contract.UsageQuantity, source contract.UsageSource) error {
	if !s.started || source != contract.UsageProviderReported {
		return errors.New("usage before start or not provider-reported")
	}
	s.units = u
	s.order = append(s.order, "usage")
	return nil
}
func (s *spoken) Audio(index int, mediaType string, data []byte) error {
	if !s.started || index != 0 || len(data) == 0 || len(data) > 49152 {
		return fmt.Errorf("invalid audio chunk of %d bytes", len(data))
	}
	if s.mediaType != "" && s.mediaType != mediaType {
		return errors.New("the media type changed mid-answer")
	}
	s.mediaType = mediaType
	s.chunks++
	if s.failAfter != 0 && s.chunks >= s.failAfter {
		return errSinkClosed
	}
	s.audio = append(s.audio, data...)
	if n := len(s.order); n == 0 || s.order[n-1] != "audio" {
		s.order = append(s.order, "audio")
	}
	return nil
}

func spokenRequest(stream bool, format contract.AudioOutputFormat) (*contract.Request, provider.Route) {
	system, question := "Answer briefly.", "What did I say?"
	earlier := "You asked about the weather."
	audio := base64.StdEncoding.EncodeToString([]byte("RIFF\x00\x00\x00\x00WAVEsynthetic"))
	wav := "audio/wav"
	request := &contract.Request{
		Modality: contract.ModalityAudio,
		Stream:   stream,
		Client:   contract.ClientRequestMetadata{APIFormat: contract.APIFormatChatCompletions},
		Input: contract.Input{Format: contract.InputMessages, Messages: []contract.Message{
			{Role: contract.RoleSystem, Content: []contract.ContentPart{{Type: contract.ContentPartText, Text: &system}}},
			{Role: contract.RoleAssistant, Content: []contract.ContentPart{{Type: contract.ContentPartText, Text: &earlier}}},
			{Role: contract.RoleUser, Content: []contract.ContentPart{
				{Type: contract.ContentPartText, Text: &question},
				{Type: contract.ContentPartAudio, Source: &contract.ContentSource{Kind: contract.ContentSourceInline, Data: &audio, MediaType: &wav}},
			}},
		}},
		AudioOutput: &contract.AudioOutputParameters{Voice: "marin", Format: format},
	}
	route := provider.Route{Provider: Slug, UpstreamModelID: "gpt-audio-1.5", DeploymentID: "dep_openai_audio_chat",
		ModelReference: "openai/gpt-audio-1.5@observed-2026-09-30"}
	return request, route
}

// pcm is synthetic 16-bit audio longer than one contract chunk, so the split
// is exercised; each sample is distinct so a reordered chunk is visible.
func pcm(samples int) []byte {
	data := make([]byte, 0, samples*2)
	for i := range samples {
		data = append(data, byte(i), byte(i>>8))
	}
	return data
}

func sseBody(frames ...string) string {
	var body strings.Builder
	for _, frame := range frames {
		body.WriteString("data: " + frame + "\n\n")
	}
	return body.String()
}

func audioFrame(transcript string, data []byte) string {
	encoded, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
		"audio": map[string]any{"id": "audio_synthetic", "transcript": transcript, "data": base64.StdEncoding.EncodeToString(data), "expires_at": 1790000000},
	}}}})
	return string(encoded)
}

// usageWithCachedAudio reports 100 prompt tokens of which 20 are cached (5 of
// those audio) and 60 are audio, and 250 completion tokens of which 200 are
// audio. The partition is input 25, cached text 15, audio 55, cached audio 5;
// output 50, audio output 200.
const usageWithCachedAudio = `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":250,"total_tokens":350,` +
	`"prompt_tokens_details":{"cached_tokens":20,"audio_tokens":60,"text_tokens":40,"cached_tokens_details":{"audio_tokens":5,"text_tokens":15}},` +
	`"completion_tokens_details":{"reasoning_tokens":0,"audio_tokens":200,"text_tokens":50,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}}}`

var partitionWithCachedAudio = []contract.UsageQuantity{
	{Unit: contract.UnitRequests, Quantity: 1},
	{Unit: contract.UnitInputTokens, Quantity: 25},
	{Unit: contract.UnitCachedInputTokens, Quantity: 15},
	{Unit: contract.UnitAudioInputTokens, Quantity: 55},
	{Unit: contract.UnitCachedAudioInputTokens, Quantity: 5},
	{Unit: contract.UnitOutputTokens, Quantity: 50},
	{Unit: contract.UnitAudioOutputTokens, Quantity: 200},
}

// readChatBody asserts the upstream received exactly the reviewed Chat
// Completions shape, and returns it for the per-test checks.
func readChatBody(t *testing.T, r *http.Request, stream bool) map[string]any {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("wrong endpoint %s %s %q", r.Method, r.URL, r.Header.Get("Content-Type"))
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("the chat body is not JSON: %v", err)
	}
	keys := []string{"audio", "messages", "modalities", "model", "stream"}
	if stream {
		keys = append(keys, "stream_options")
	}
	got := make([]string, 0, len(body))
	for key := range body {
		got = append(got, key)
	}
	if len(got) != len(keys) {
		t.Errorf("body keys = %v, want exactly %v; an unset control must not be invented", got, keys)
	}
	for _, key := range keys {
		if _, ok := body[key]; !ok {
			t.Errorf("body lacks %q", key)
		}
	}
	if body["model"] != "gpt-audio-1.5" || body["stream"] != stream || !reflect.DeepEqual(body["modalities"], []any{"text", "audio"}) {
		t.Errorf("model/stream/modalities = %v %v %v", body["model"], body["stream"], body["modalities"])
	}
	if stream && !reflect.DeepEqual(body["stream_options"], map[string]any{"include_usage": true}) {
		t.Errorf("stream_options = %v", body["stream_options"])
	}
	messages, _ := body["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %v", body["messages"])
	}
	wantMessages := []any{
		map[string]any{"role": "system", "content": "Answer briefly."},
		map[string]any{"role": "assistant", "content": "You asked about the weather."},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "What did I say?"},
			map[string]any{"type": "input_audio", "input_audio": map[string]any{
				"data": base64.StdEncoding.EncodeToString([]byte("RIFF\x00\x00\x00\x00WAVEsynthetic")), "format": "wav"}},
		}},
	}
	if !reflect.DeepEqual(messages, wantMessages) {
		t.Errorf("messages = %#v", messages)
	}
	return body
}

func TestStreamedSpokenAnswerCarriesAudioTranscriptAndAudioTokens(t *testing.T) {
	first, second := pcm(30000), pcm(9000)
	a := adapter(t, func(w http.ResponseWriter, r *http.Request) {
		body := readChatBody(t, r, true)
		if !reflect.DeepEqual(body["audio"], map[string]any{"voice": "marin", "format": "pcm16"}) {
			t.Errorf("audio = %v", body["audio"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody(
			`{"choices":[{"index":0,"delta":{"role":"assistant","content":null}}]}`,
			audioFrame("You said", first),
			audioFrame(" hello.", second),
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			usageWithCachedAudio,
			"[DONE]",
		))
	})
	r, route := spokenRequest(true, contract.AudioOutputPCM)
	call, err := a.Translate(r, route)
	if err != nil {
		t.Fatal(err)
	}
	out := &spoken{}
	outcome, err := a.Stream(context.Background(), call, out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.channels[contract.ChannelOutputAudioTranscript] != "You said hello." {
		t.Errorf("transcript = %q", out.channels[contract.ChannelOutputAudioTranscript])
	}
	if _, text := out.channels[contract.ChannelOutputText]; text {
		t.Error("the transcript of spoken output was also delivered as written text")
	}
	if out.mediaType != "audio/pcm" || !bytes.Equal(out.audio, append(append([]byte(nil), first...), second...)) {
		t.Errorf("audio %q of %d bytes did not survive the chunking", out.mediaType, len(out.audio))
	}
	if out.chunks != 3 { // 60000 bytes is two chunks; 18000 is one
		t.Errorf("chunks = %d", out.chunks)
	}
	if !reflect.DeepEqual(outcome.Units, partitionWithCachedAudio) || !reflect.DeepEqual(out.units, partitionWithCachedAudio) {
		t.Errorf("units = %+v, emitted %+v", outcome.Units, out.units)
	}
	if outcome.UsageSource != contract.UsageProviderReported || outcome.FinishReason != contract.FinishStop || outcome.KeyID == "" {
		t.Errorf("outcome = %+v", outcome)
	}
}

func TestWholeSpokenAnswerIsChunkedWithItsFormatsMediaType(t *testing.T) {
	for format, mediaType := range map[contract.AudioOutputFormat]string{
		"wav": "audio/wav", "mp3": "audio/mpeg", "flac": "audio/flac", "opus": "audio/ogg", "pcm": "audio/pcm",
	} {
		t.Run(string(format), func(t *testing.T) {
			wire := map[contract.AudioOutputFormat]string{"wav": "wav", "mp3": "mp3", "flac": "flac", "opus": "opus", "pcm": "pcm16"}[format]
			data := pcm(60000)
			a := adapter(t, func(w http.ResponseWriter, r *http.Request) {
				body := readChatBody(t, r, false)
				if !reflect.DeepEqual(body["audio"], map[string]any{"voice": "marin", "format": wire}) {
					t.Errorf("audio = %v", body["audio"])
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-synthetic","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":null,`+
					`"audio":{"id":"audio_synthetic","expires_at":1790000000,"data":%q,"transcript":"You said hello."}},"finish_reason":"stop"}],`+
					`"usage":{"prompt_tokens":100,"completion_tokens":250,"prompt_tokens_details":{"cached_tokens":0,"audio_tokens":60},"completion_tokens_details":{"reasoning_tokens":0,"audio_tokens":200}}}`,
					base64.StdEncoding.EncodeToString(data))
			})
			r, route := spokenRequest(false, format)
			call, err := a.Translate(r, route)
			if err != nil {
				t.Fatal(err)
			}
			out := &spoken{}
			outcome, err := a.Stream(context.Background(), call, out, nil)
			if err != nil {
				t.Fatal(err)
			}
			if out.mediaType != mediaType || !bytes.Equal(out.audio, data) || out.chunks != 3 {
				t.Errorf("audio %q, %d chunks, intact=%t", out.mediaType, out.chunks, bytes.Equal(out.audio, data))
			}
			if out.channels[contract.ChannelOutputAudioTranscript] != "You said hello." || out.channels[contract.ChannelOutputText] != "" {
				t.Errorf("channels = %v", out.channels)
			}
			// Without cached_tokens_details no cached token is attributed to audio.
			want := []contract.UsageQuantity{
				{Unit: contract.UnitRequests, Quantity: 1},
				{Unit: contract.UnitInputTokens, Quantity: 40},
				{Unit: contract.UnitAudioInputTokens, Quantity: 60},
				{Unit: contract.UnitOutputTokens, Quantity: 50},
				{Unit: contract.UnitAudioOutputTokens, Quantity: 200},
			}
			if !reflect.DeepEqual(outcome.Units, want) {
				t.Errorf("units = %+v, want %+v", outcome.Units, want)
			}
			if strings.Join(out.order, ",") != "start,delta:output_audio_transcript,audio,usage" {
				t.Errorf("event order = %v", out.order)
			}
		})
	}
}

func TestAudioUsagePartition(t *testing.T) {
	parse := func(raw string) *chatUsage {
		var usage chatUsage
		if err := json.Unmarshal([]byte(raw), &usage); err != nil {
			t.Fatal(err)
		}
		return &usage
	}
	var wrapped struct {
		Usage *chatUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(usageWithCachedAudio), &wrapped); err != nil {
		t.Fatal(err)
	}
	units, ok := wrapped.Usage.units()
	if !ok || !reflect.DeepEqual(units, partitionWithCachedAudio) {
		t.Fatalf("units = %+v, %t", units, ok)
	}
	// The partition sums back to the provider's own totals: nothing is billed
	// twice and nothing is lost.
	sum := map[bool]int{}
	for _, u := range units {
		switch u.Unit {
		case contract.UnitInputTokens, contract.UnitCachedInputTokens, contract.UnitAudioInputTokens, contract.UnitCachedAudioInputTokens:
			sum[true] += u.Quantity
		case contract.UnitOutputTokens, contract.UnitReasoningTokens, contract.UnitAudioOutputTokens:
			sum[false] += u.Quantity
		}
	}
	if sum[true] != 100 || sum[false] != 250 {
		t.Errorf("partition sums to %d/%d, want 100/250", sum[true], sum[false])
	}
	// Audio is never also reported by duration.
	for _, u := range units {
		if u.Unit == contract.UnitAudioInputMilliseconds || u.Unit == contract.UnitAudioOutputMilliseconds {
			t.Errorf("the same audio was reported as %s too", u.Unit)
		}
	}
	textOnly, ok := parse(`{"prompt_tokens":12,"completion_tokens":30,"completion_tokens_details":{"reasoning_tokens":4}}`).units()
	if !ok || !reflect.DeepEqual(textOnly, []contract.UsageQuantity{
		{Unit: contract.UnitRequests, Quantity: 1}, {Unit: contract.UnitInputTokens, Quantity: 12},
		{Unit: contract.UnitOutputTokens, Quantity: 26}, {Unit: contract.UnitReasoningTokens, Quantity: 4},
	}) {
		t.Errorf("text-only usage = %+v, %t", textOnly, ok)
	}
	for name, raw := range map[string]string{
		"no prompt count":              `{"completion_tokens":3}`,
		"no completion count":          `{"prompt_tokens":3}`,
		"audio beyond the prompt":      `{"prompt_tokens":10,"completion_tokens":1,"prompt_tokens_details":{"audio_tokens":11}}`,
		"audio beyond the completion":  `{"prompt_tokens":10,"completion_tokens":1,"completion_tokens_details":{"audio_tokens":2}}`,
		"cached audio beyond audio":    `{"prompt_tokens":10,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":3,"audio_tokens":1,"cached_tokens_details":{"audio_tokens":2}}}`,
		"cached audio beyond cached":   `{"prompt_tokens":10,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":1,"audio_tokens":4,"cached_tokens_details":{"audio_tokens":2}}}`,
		"negative completion":          `{"prompt_tokens":10,"completion_tokens":-1}`,
		"cached and audio overlap too": `{"prompt_tokens":10,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":6,"audio_tokens":6}}`,
	} {
		if units, ok := parse(raw).units(); ok {
			t.Errorf("%s: accepted as %+v; an inconsistent report must not be clamped into a bill", name, units)
		}
	}
}

func TestSpokenAnswerFailuresKeepWhatWasMeasured(t *testing.T) {
	route := func(body string) (*Adapter, *provider.Call) {
		a := adapter(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, body)
		})
		r, rt := spokenRequest(true, contract.AudioOutputPCM)
		call, err := a.Translate(r, rt)
		if err != nil {
			t.Fatal(err)
		}
		return a, call
	}

	t.Run("cut after usage", func(t *testing.T) {
		a, call := route(sseBody(audioFrame("Hi", pcm(100)), usageWithCachedAudio)) // no [DONE]
		outcome, err := a.Stream(context.Background(), call, &spoken{}, nil)
		var upstream provider.ErrUpstream
		if !errors.As(err, &upstream) {
			t.Fatalf("a truncated stream was reported as %v", err)
		}
		if !reflect.DeepEqual(outcome.Units, partitionWithCachedAudio) || outcome.UsageSource != contract.UsageProviderReported {
			t.Errorf("measured units were lost with the failure: %+v", outcome)
		}
	})

	t.Run("downstream gone mid-audio", func(t *testing.T) {
		a, call := route(sseBody(usageWithCachedAudio, audioFrame("Hi", pcm(60000)), "[DONE]"))
		out := &spoken{failAfter: 2}
		outcome, err := a.Stream(context.Background(), call, out, nil)
		if !errors.Is(err, errSinkClosed) {
			t.Fatalf("err = %v", err)
		}
		if !reflect.DeepEqual(outcome.Units, partitionWithCachedAudio) {
			t.Errorf("units = %+v", outcome.Units)
		}
	})

	t.Run("error after a 200", func(t *testing.T) {
		a, call := route(sseBody(audioFrame("Hi", pcm(100)),
			`{"error":{"type":"server_error","code":null,"message":"upstream failed for Bearer `+fakeKey+`"}}`))
		out := &spoken{}
		_, err := a.Stream(context.Background(), call, out, nil)
		var upstream provider.ErrUpstream
		if !errors.As(err, &upstream) || upstream.Code != contract.CodeProviderError || upstream.Category != contract.UpstreamServerError {
			t.Fatalf("err = %v", err)
		}
		if upstream.Passthrough == nil || upstream.Passthrough.Message == nil || strings.Contains(*upstream.Passthrough.Message, fakeKey) {
			t.Errorf("the diagnostic was lost or carries the key: %+v", upstream.Passthrough)
		}
		if len(out.audio) != 200 {
			t.Error("audio delivered before the failure was not delivered")
		}
	})

	for name, body := range map[string]string{
		"inconsistent usage": sseBody(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"completion_tokens_details":{"audio_tokens":5}}}`, "[DONE]"),
		"audio not base64":   sseBody(`{"choices":[{"index":0,"delta":{"audio":{"data":"not base64!"}}}]}`, "[DONE]"),
		"unreadable frame":   sseBody(`{"choices":`, "[DONE]"),
	} {
		t.Run(name, func(t *testing.T) {
			a, call := route(body)
			_, err := a.Stream(context.Background(), call, &spoken{}, nil)
			var upstream provider.ErrUpstream
			if !errors.As(err, &upstream) || upstream.Code != contract.CodeProviderError {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestOversizedSpokenAnswerIsRefused(t *testing.T) {
	data := make([]byte, maxAudioBytes+1)
	a := adapter(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"choices":[{"index":0,"message":{"audio":{"data":%q,"transcript":"x"}},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			base64.StdEncoding.EncodeToString(data))
	})
	r, route := spokenRequest(false, "wav")
	call, _ := a.Translate(r, route)
	out := &spoken{}
	outcome, err := a.Stream(context.Background(), call, out, nil)
	var upstream provider.ErrUpstream
	if !errors.As(err, &upstream) || upstream.Code != contract.CodeProviderError {
		t.Fatalf("err = %v", err)
	}
	if len(out.audio) != 0 || len(outcome.Units) == 0 {
		t.Errorf("delivered %d bytes; units %+v", len(out.audio), outcome.Units)
	}
	// Positive control: exactly the ceiling is delivered whole.
	data = data[:maxAudioBytes]
	a = adapter(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"choices":[{"index":0,"message":{"audio":{"data":%q,"transcript":"x"}},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			base64.StdEncoding.EncodeToString(data))
	})
	call, _ = a.Translate(r, route)
	out = &spoken{}
	if _, err := a.Stream(context.Background(), call, out, nil); err != nil || len(out.audio) != maxAudioBytes {
		t.Fatalf("the contract's ceiling was refused: %v (%d bytes)", err, len(out.audio))
	}
}

func TestUnspeakableChatRequestsAreRefusedBeforeSending(t *testing.T) {
	a := adapter(t, func(http.ResponseWriter, *http.Request) { t.Error("a refused request reached the upstream") })
	text := "hi"
	cases := map[string]struct {
		mutate func(*contract.Request)
		param  string
	}{
		"text chat":      {func(r *contract.Request) { r.AudioOutput = nil }, "audioOutput"},
		"text modality":  {func(r *contract.Request) { r.Modality = contract.ModalityText }, "modality"},
		"streamed wav":   {func(r *contract.Request) { r.Stream, r.AudioOutput.Format = true, "wav" }, "audioOutput.format"},
		"unknown format": {func(r *contract.Request) { r.AudioOutput.Format = "aac" }, "audioOutput.format"},
		"no voice":       {func(r *contract.Request) { r.AudioOutput.Voice = "" }, "audioOutput.voice"},
		"reasoning":      {func(r *contract.Request) { r.Reasoning = &contract.ReasoningParameters{Effort: "low"} }, "reasoning.effort"},
		"tools":          {func(r *contract.Request) { r.Tools = []contract.ToolDefinition{{Type: "function", Name: "f"}} }, "tools"},
		"structured": {func(r *contract.Request) {
			r.ResponseFormat = &contract.ResponseFormat{Type: contract.ResponseFormatJSONObject}
		}, "responseFormat"},
		"top_k": {func(r *contract.Request) { k := 3; r.Sampling.TopK = &k }, "sampling.topK"},
		"speech": {func(r *contract.Request) {
			r.Speech = &contract.SpeechParameters{Voice: "alloy", ResponseFormat: "mp3"}
		}, "speech"},
		"text input": {func(r *contract.Request) { r.Input = contract.Input{Format: contract.InputText, Text: &text} }, "input.format"},
		"tool turn": {func(r *contract.Request) {
			id := "c1"
			r.Input.Messages[1].Role, r.Input.Messages[1].ToolCallID = contract.RoleTool, &id
		}, "input.messages[1].role"},
		"assistant calls": {func(r *contract.Request) {
			r.Input.Messages[1].ToolCalls = []contract.ToolCall{{ID: "c", Name: "f", Arguments: "{}"}}
		}, "input.messages[1].toolCalls"},
		"image part": {func(r *contract.Request) { r.Input.Messages[2].Content[1].Type = contract.ContentPartImage }, "input.messages[2].content[1]"},
		"flac input audio": {func(r *contract.Request) {
			flac := "audio/flac"
			r.Input.Messages[2].Content[1].Source.MediaType = &flac
		}, "input.messages[2].content[1].mediaType"},
		"remote audio": {func(r *contract.Request) {
			url := "https://example.com/a.wav"
			r.Input.Messages[2].Content[1].Source = &contract.ContentSource{Kind: contract.ContentSourceURL, URL: &url}
		}, "input.messages[2].content[1]"},
		"bad base64": {func(r *contract.Request) {
			bad := "not base64!"
			r.Input.Messages[2].Content[1].Source.Data = &bad
		}, "input.messages[2].content[1]"},
		"assistant audio": {func(r *contract.Request) {
			r.Input.Messages[1].Content = append(r.Input.Messages[1].Content, r.Input.Messages[2].Content[1])
		}, "input.messages[1].content[1]"},
		"transcription format": {func(r *contract.Request) { r.Client.APIFormat = contract.APIFormatAudioTranscriptions }, "audioOutput"},
	}
	for name, c := range cases {
		r, route := spokenRequest(false, "wav")
		c.mutate(r)
		_, err := a.Translate(r, route)
		var unsupported provider.ErrUnsupported
		if !errors.As(err, &unsupported) || unsupported.Param != c.param {
			t.Errorf("%s: err = %v, want a refusal naming %q", name, err, c.param)
		}
	}
	// Positive control: the unmutated request, and every sampling control the
	// caller set, is translated and sent as given.
	r, route := spokenRequest(true, contract.AudioOutputPCM)
	temperature, maxTokens, seed := 0.4, 512, 7
	r.Sampling = contract.SamplingParameters{Temperature: &temperature, Seed: &seed, StopSequences: []string{"END"}}
	r.MaxOutputTokens = &maxTokens
	r.ResponseFormat = &contract.ResponseFormat{Type: contract.ResponseFormatText}
	call, err := a.Translate(r, route)
	if err != nil {
		t.Fatalf("a speakable request was refused: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(call.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["temperature"] != 0.4 || body["max_completion_tokens"] != 512.0 || body["seed"] != 7.0 || !reflect.DeepEqual(body["stop"], []any{"END"}) {
		t.Errorf("sampling controls were not sent as given: %v", body)
	}
	if _, invented := body["response_format"]; invented {
		t.Error("a plain-text response format was sent as a structured one")
	}
	if !call.Stream || call.Header.Get("Accept") != "text/event-stream" {
		t.Errorf("stream call = %+v", call)
	}
}

func TestSpokenChatRefusalsAreClassifiedAndRedacted(t *testing.T) {
	cases := []struct {
		status int
		body   string
		code   contract.ErrorCode
	}{
		{429, `{"error":{"type":"insufficient_quota","code":"insufficient_quota","message":"quota"}}`, contract.CodeProviderBillingRefused},
		{429, `{"error":{"type":"rate_limit_error","code":"slow_down","message":"slow down"}}`, contract.CodeRateLimited},
		{401, `{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"Incorrect API key provided: ` + fakeKey + `"}}`, contract.CodeProviderCredentialInvalid},
		{400, `{"error":{"type":"invalid_request_error","code":"invalid_value","param":"audio.voice","message":"Invalid value: 'nobody'"}}`, contract.CodeInvalidRequest},
		{404, `{"error":{"type":"invalid_request_error","code":"model_not_found","message":"no"}}`, contract.CodeModelNotFound},
		{503, `{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"busy"}}`, contract.CodeProviderOverloaded},
	}
	for _, c := range cases {
		a := adapter(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.body)
		})
		r, route := spokenRequest(true, contract.AudioOutputPCM)
		call, _ := a.Translate(r, route)
		out := &spoken{}
		outcome, err := a.Stream(context.Background(), call, out, nil)
		var upstream provider.ErrUpstream
		if !errors.As(err, &upstream) || upstream.Code != c.code {
			t.Errorf("%d %s: %v", c.status, c.body, err)
			continue
		}
		if out.started || outcome.KeyID == "" {
			t.Errorf("%d: started=%t key=%q", c.status, out.started, outcome.KeyID)
		}
		if upstream.Passthrough == nil || upstream.Passthrough.Message == nil || strings.Contains(*upstream.Passthrough.Message, fakeKey) {
			t.Errorf("%d: the diagnostic was lost or carries the key: %+v", c.status, upstream.Passthrough)
		}
	}
}

func TestSpokenStreamCancellationReachesTheUpstream(t *testing.T) {
	run := func(cancelMidway bool) (error, bool) {
		released := make(chan struct{})
		a := adapter(t, func(w http.ResponseWriter, r *http.Request) {
			defer close(released)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sseBody(audioFrame("Hi", pcm(100))))
			w.(http.Flusher).Flush()
			if cancelMidway {
				<-r.Context().Done()
				return
			}
			_, _ = io.WriteString(w, sseBody(usageWithCachedAudio, "[DONE]"))
		})
		r, route := spokenRequest(true, contract.AudioOutputPCM)
		call, _ := a.Translate(r, route)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := &cancelOnAudio{spoken: spoken{}, cancel: map[bool]context.CancelFunc{true: cancel}[cancelMidway]}
		_, err := a.Stream(ctx, call, out, nil)
		select {
		case <-released:
			return err, true
		case <-time.After(5 * time.Second):
			return err, false
		}
	}
	err, released := run(true)
	if !errors.Is(err, context.Canceled) || !released {
		t.Fatalf("cancelled: err=%v, upstream released=%t", err, released)
	}
	// Control: the same stream uninterrupted completes, so "released" above is
	// the cancellation and not merely the handler returning.
	if err, released := run(false); err != nil || !released {
		t.Fatalf("uninterrupted: err=%v released=%t", err, released)
	}
}

type cancelOnAudio struct {
	spoken
	cancel context.CancelFunc
}

func (c *cancelOnAudio) Audio(index int, mediaType string, data []byte) error {
	if err := c.spoken.Audio(index, mediaType, data); err != nil {
		return err
	}
	if c.cancel != nil {
		c.cancel()
	}
	return nil
}

func TestTheAudioAdapterDeclaresTranscriptionAndSpokenChat(t *testing.T) {
	a := adapter(t, func(http.ResponseWriter, *http.Request) {})
	if got := a.APIFormats(); !reflect.DeepEqual(got, []contract.APIFormat{contract.APIFormatAudioTranscriptions, contract.APIFormatChatCompletions}) {
		t.Fatalf("declares %v", got)
	}
	// A transcription carrying audioOutput is refused rather than transcribed.
	r, route := transcriptionRequest("gpt-transcribe", "audio/wav", []byte("synthetic"))
	r.AudioOutput = &contract.AudioOutputParameters{Voice: "marin", Format: "wav"}
	var unsupported provider.ErrUnsupported
	if _, err := a.Translate(r, route); !errors.As(err, &unsupported) || unsupported.Param != "audioOutput" {
		t.Fatalf("err = %v", err)
	}
}
