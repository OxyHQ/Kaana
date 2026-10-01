// Package openaicompat adapts the OpenAI Chat Completions wire protocol.
//
// The implementation is protocol-shaped rather than provider-shaped: OpenAI,
// Together, xAI, Cerebras, Hyperbolic, DigitalOcean, OpenRouter, Alibaba Model
// Studio and Cloudflare Workers AI all use it through configuration and
// conformance registration instead of separate adapters.
// Provider-specific wire policy still belongs here: Kaana's mandatory privacy
// and parameter-support preferences for OpenRouter are added by Translate and
// are never exposed as caller or operator configuration.
//
// The raw upstream stream never crosses Kaana's boundary. This adapter
// normalizes events and usage, propagates cancellation, classifies provider
// failures and requests streamed usage. It sends no sampling default the
// caller omitted: the selected deployment's own default remains authoritative.
// Conformance uses a fake upstream that speaks the real wire format.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// Config describes one provider that speaks this protocol.
type Config struct {
	// Provider is the catalogue slug this instance serves.
	Provider contract.ProviderSlug
	// BaseURL is the provider's API root, e.g. https://api.openai.com/v1.
	BaseURL string
	// Declarations are Kaana's own credentials for this provider, in the order
	// they were declared. The
	// secrets are decrypted from Kaana's PostgreSQL credential store at runtime,
	// never read from a request or provider-key environment variable, never
	// written to a file in this repository, and never written to a log, an error
	// or a usage record.
	//
	// It is a list because one provider account's capacity is not the same
	// thing as one provider's capacity: when the account behind a key has
	// nothing left, the next key is a different account that does. An empty
	// list is a supported state — the adapter reports itself unconfigured
	// rather than failing at the first request.
	Declarations []provider.KeyDeclaration
	// Keys is how this pool behaves when a provider says something about one of
	// its credentials. Its zero value is the conservative one.
	Keys provider.KeyPolicy
	// Headers are extra non-secret headers the provider expects. OpenRouter's
	// attribution headers are the reason this exists.
	Headers map[string]string
	// HTTPClient is optional. It carries no global timeout, because one would
	// bound the body and the body is the generation. What it does carry, when
	// cmd/kaana builds it, is a header deadline: the half of the exchange that
	// is never legitimately unbounded. A nil client answers nothing at all and
	// waits forever, so it is for tests only — see provider.BoundResponseHeaders.
	HTTPClient *http.Client
	// StreamIdleTimeout bounds how long a streamed response may go without a
	// real frame once its headers are in. Zero takes
	// provider.DefaultStreamIdleTimeout.
	StreamIdleTimeout time.Duration
}

// Adapter implements provider.Adapter for one OpenAI-compatible provider.
type Adapter struct {
	decisions decisionReview
	// decisionTimeout overrides defaultDecisionDeadline; only tests set it.
	decisionTimeout time.Duration

	config      Config
	client      *http.Client
	credentials *provider.KeyPool
}

func (a *Adapter) PlatformCredentials() *provider.KeyPool { return a.credentials }

// New builds an adapter, refusing a configuration that could not serve.
func New(config Config) (*Adapter, error) {
	if !config.Provider.Valid() {
		return nil, fmt.Errorf("openaicompat: %q is not a provider slug", config.Provider)
	}
	if config.BaseURL == "" {
		return nil, fmt.Errorf("openaicompat: %s has no base URL", config.Provider)
	}
	if err := providerconfig.ValidateEndpointIdentity(config.Provider, config.BaseURL); err != nil {
		return nil, fmt.Errorf("openaicompat: %s: %w", config.Provider, err)
	}
	// The quota-header mapping comes from this package's own table rather than
	// from the caller: which header means "credits remaining" is knowledge
	// about a provider's wire protocol, and an operator who mapped a rate-limit
	// header onto it would retire every key the first time the provider
	// throttled one.
	credentials, err := provider.NewKeyPool(config.Provider, config.Declarations, config.Keys, quotaHeadersFor(config.Provider))
	if err != nil {
		return nil, err
	}
	client := provider.RefuseRedirects(config.HTTPClient)
	config.BaseURL = strings.TrimSuffix(config.BaseURL, "/")
	return &Adapter{config: config, client: client, credentials: credentials}, nil
}

// quotaHeadersFor is what each provider speaking this protocol declares about
// its own response headers.
//
// It is EMPTY, and the emptiness is asserted with an exact count in
// credential_test.go rather than left to be noticed. No provider served here
// documents a remaining-credits header on a chat-completions response, and no
// live provider call has been made from this repository to discover one — so
// there is nothing to declare, and a plausible guess is precisely the failure
// this mapping exists to prevent. Every provider's quota state is therefore
// learned from the provider's own refusal, and is `unknown` until then.
//
// Adding an entry is the way a verified header signal arrives. It is a
// deliberate act with a count to move, not a line to append.
//
// # This was measured on 2026-08-23, and there was nothing to declare
//
// A real chat completion was sent to each provider this build serves, with that
// account's own key, and every response header matching credit/quota/balance/
// limit/remaining/reset was read:
//
//	openrouter  NO such header at all
//	groq        x-ratelimit-{limit,remaining}-{requests,tokens},
//	            x-ratelimit-reset-requests: 1m26.4s, -reset-tokens: 570ms
//	xai         x-ratelimit-{limit,remaining}-{requests,tokens}
//	cerebras    not measured: the account answers 402 before a completion
//
// Groq's and xAI's are BURST limits — the resets are in seconds and minutes, and
// the remaining counts refill. Mapping either onto SignalRemainingCredits is the
// exact mistake the QuotaSignal doc refuses: it would retire a healthy key every
// time a provider throttled one, and the pool would empty for no reason. A
// burst limit is already handled, as a 429, by Throttled and the retry path.
//
// So the emptiness below is a RESULT, not an omission. Anyone tempted to fill it
// from a header name that looks right should re-run that measurement first.
//
// One real credit signal does exist and is NOT a header: OpenRouter returns
// `usage.cost` in the response BODY (measured: 0.000080255 on a 91-token
// completion). Its account-level `GET /api/v1/credits` is not a substitute —
// on the same day it answered `total_credits: 0, total_usage: 0` both before and
// after a completion that really was billed. Anything that rations against spend
// has to accumulate the body figure; no header will tell it.
func quotaHeadersFor(slug contract.ProviderSlug) provider.QuotaHeaders {
	return declaredQuotaHeaders[slug]
}

var declaredQuotaHeaders = map[contract.ProviderSlug]provider.QuotaHeaders{}

// Provider implements provider.Adapter.
func (a *Adapter) Provider() contract.ProviderSlug { return a.config.Provider }

// APIFormats implements provider.Adapter. Embeddings and speech are provider
// specific endpoints; the table in providerconfig names which slug has them.
func (a *Adapter) APIFormats() []contract.APIFormat {
	return providerconfig.ExecutableAPIFormats(a.config.Provider, providerconfig.ProtocolOpenAICompatible)
}

// Translate implements provider.Adapter.
//
// Every refusal below happens before a single byte is sent upstream, which is
// the whole reason translation is a separate, pure method: a request this
// protocol cannot express must cost nothing.
func (a *Adapter) Translate(request *contract.Request, route provider.Route) (*provider.Call, error) {
	if request.Client.APIFormat == contract.APIFormatDecisions || request.Input.Format == contract.InputDecisions {
		return a.translateDecisions(request, route)
	}
	if a.Provider() == "typesafe" {
		return nil, decisionsUnavailable()
	}
	if request.AudioOutput != nil {
		// Spoken output is audio events plus a transcript channel. This adapter
		// produces it only on a deployment that answers aloud — OpenRouter's
		// rows for OpenAI's audio chat models — through the shared spoken wire
		// (internal/provider/spokenchat). Everywhere else it writes text, so
		// the refusal is here as well as in the executor's gate, and cannot
		// rest on the modality check below staying where it is.
		if !providerconfig.SpeaksAloud(a.config.Provider, providerconfig.ProtocolOpenAICompatible, route.UpstreamModelID) {
			return nil, provider.ErrUnsupported{
				Code:   contract.CodeUnsupportedModality,
				Param:  "audioOutput",
				Detail: "this deployment produces text; spoken output needs a deployment that answers aloud",
			}
		}
		return a.translateSpoken(request, route)
	}
	if request.Reasoning != nil && (request.Client.APIFormat == contract.APIFormatAudioSpeech || request.Modality != contract.ModalityText) {
		// Speech and embeddings have no reasoning step to control. Accepting
		// the effort and dropping it would report a control that did nothing.
		return nil, provider.ErrUnsupported{
			Code:   contract.CodeInvalidRequest,
			Param:  "reasoning.effort",
			Detail: "reasoning effort applies only to text generation",
		}
	}
	if request.Client.APIFormat == contract.APIFormatAudioSpeech {
		return a.translateSpeech(request, route)
	}
	if a.config.Provider == "xai" && route.UpstreamModelID == "tts" {
		return nil, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: "modality", Detail: "the speech deployment requires audio_speech"}
	}
	if request.Modality == contract.ModalityEmbedding {
		if a.config.Provider != "siliconflow" {
			return nil, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: "modality", Detail: "this deployment does not expose an embeddings endpoint"}
		}
		if request.Stream {
			return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "stream", Detail: "embeddings are non-streaming"}
		}
		var input any
		switch request.Input.Format {
		case contract.InputText:
			input = derefString(request.Input.Text)
		case contract.InputTextBatch:
			input = request.Input.Texts
		default:
			return nil, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: "input.format", Detail: "embeddings require text or text_batch input"}
		}
		body, err := json.Marshal(embeddingRequest{Model: route.UpstreamModelID, Input: input, Dimensions: 1024})
		if err != nil {
			return nil, fmt.Errorf("openaicompat: encoding embedding request: %w", err)
		}
		headers := make(http.Header, len(a.config.Headers))
		for name, value := range a.config.Headers {
			headers.Set(name, value)
		}
		return &provider.Call{Route: route, Method: http.MethodPost, URL: a.config.BaseURL + "/embeddings", Body: body, Header: headers}, nil
	}
	if request.Modality != contract.ModalityText {
		return nil, provider.ErrUnsupported{
			Code:   contract.CodeUnsupportedModality,
			Param:  "modality",
			Detail: fmt.Sprintf("chat completions produces text; %q needs a different upstream endpoint", request.Modality),
		}
	}
	if request.Input.Format != contract.InputMessages {
		return nil, provider.ErrUnsupported{
			Code:   contract.CodeUnsupportedModality,
			Param:  "input.format",
			Detail: fmt.Sprintf("chat completions takes a conversation; %q is an embedding input", request.Input.Format),
		}
	}

	messages := make([]chatMessage, 0, len(request.Input.Messages))
	for index, message := range request.Input.Messages {
		translated, err := translateMessage(message)
		if err != nil {
			return nil, annotate(err, fmt.Sprintf("input.messages[%d]", index))
		}
		messages = append(messages, translated)
	}

	body := chatRequest{
		Model:            route.UpstreamModelID,
		Messages:         messages,
		Stream:           request.Stream,
		MaxTokens:        request.MaxOutputTokens,
		Temperature:      request.Sampling.Temperature,
		TopP:             request.Sampling.TopP,
		FrequencyPenalty: request.Sampling.FrequencyPenalty,
		PresencePenalty:  request.Sampling.PresencePenalty,
		Seed:             request.Sampling.Seed,
		Stop:             request.Sampling.StopSequences,
	}
	if a.config.Provider == "openrouter" {
		// These are Kaana's non-negotiable OpenRouter controls, not caller or
		// operator preferences. Keeping them out of Config and the normalized
		// contract leaves no merge path on which a weaker value could win.
		body.Provider = &openRouterProviderPolicy{
			ZDR:               true,
			DataCollection:    "deny",
			RequireParameters: true,
		}
	}
	if request.Stream {
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if request.Reasoning != nil {
		if err := refuseUnstatedEffort(a.config.Provider, route.UpstreamModelID, request.Reasoning.Effort); err != nil {
			return nil, err
		}
		effort := string(request.Reasoning.Effort)
		switch reasoningDialectFor(a.config.Provider) {
		case reasoningObject:
			body.Reasoning = &openRouterReasoning{Effort: effort}
		case reasoningEffortField:
			body.ReasoningEffort = &effort
		default:
			// This protocol has no single reasoning field: `reasoning_effort`
			// is OpenAI's, adopted by some compatible providers and silently
			// ignored by others. Sending it where nobody reviewed the provider's
			// documentation could report an effort that changed nothing.
			return nil, provider.ErrUnsupported{
				Code:   contract.CodeInvalidRequest,
				Param:  "reasoning.effort",
				Detail: "this provider's chat completions dialect has no reviewed reasoning-effort control",
			}
		}
	}
	if request.Sampling.TopK != nil {
		// top_k has no representation in this protocol. Dropping it silently
		// would change what the model does while reporting success, so the
		// request is refused with the field named.
		return nil, provider.ErrUnsupported{
			Code:   contract.CodeInvalidRequest,
			Param:  "sampling.topK",
			Detail: "chat completions has no top_k parameter",
		}
	}
	for _, tool := range request.Tools {
		body.Tools = append(body.Tools, chatTool{
			Type: "function",
			Function: chatFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.Parameters,
				Strict:      tool.Strict,
			},
		})
	}
	if choice := request.ToolChoice; choice != nil {
		switch {
		case choice.Mode != nil:
			body.ToolChoice = string(*choice.Mode)
		case choice.Function != nil:
			body.ToolChoice = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": choice.Function.Name},
			}
		}
	}
	if format := request.ResponseFormat; format != nil {
		translated, err := translateResponseFormat(*format)
		if err != nil {
			return nil, err
		}
		body.ResponseFormat = translated
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: encoding the upstream request: %w", err)
	}
	if err := a.refuseUnacceptedParameters(encoded, route); err != nil {
		return nil, err
	}

	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("Accept", map[bool]string{true: "text/event-stream", false: "application/json"}[request.Stream])
	for name, value := range a.config.Headers {
		header.Set(name, value)
	}

	return &provider.Call{
		Route:  route,
		Method: http.MethodPost,
		URL:    a.config.BaseURL + "/chat/completions",
		Body:   encoded,
		Header: header,
		Stream: request.Stream,
	}, nil
}

// refuseUnacceptedParameters refuses a control the route's published
// accepted-parameter set says its upstream does not take.
//
// It reads the encoded BODY about to be sent rather than the contract request,
// so it judges exactly what the upstream would see — a `text` response format
// sends nothing and is not a response_format — and it serves the text and the
// spoken wire alike. A route whose set is unknown refuses nothing: absence is
// "nobody said", never "nothing is accepted".
//
// This matters most for OpenRouter. Every request there requires zero data
// retention AND that the serving endpoint accept every parameter sent
// (`require_parameters`), so a parameter no zero-retention endpoint of the
// model lists can never be served: OpenRouter answers 404 after the request
// has crossed the network. Dropping the parameter instead would change what
// the model does while reporting success.
func (a *Adapter) refuseUnacceptedParameters(encoded []byte, route provider.Route) error {
	if route.AcceptedParameters == nil {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return fmt.Errorf("openaicompat: reading back the upstream request: %w", err)
	}
	var sent []provider.RequestParameter
	for word, parameter := range wireParameters {
		if value, present := fields[word]; present && string(value) != "null" {
			sent = append(sent, parameter)
		}
	}
	parameter, refused := route.UnacceptedParameter(sent...)
	if !refused {
		return nil
	}
	detail := fmt.Sprintf("%s reports that this model does not accept %s; omit it to use the model's own behaviour", a.config.Provider, parameter)
	if a.config.Provider == "openrouter" {
		detail = fmt.Sprintf("this model's zero-data-retention endpoints on openrouter do not accept %s; omit it to use the model's own behaviour", parameter)
	}
	return provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: string(parameter), Detail: detail}
}

// wireParameters maps the Chat Completions body fields this adapter (and the
// shared spoken wire) sends onto the control each expresses. Every other body
// field (`model`, `messages`, `stream`, `stream_options`, `provider`,
// `modalities`, `audio`) is not a caller control the accepted-parameter set
// states.
var wireParameters = map[string]provider.RequestParameter{
	"max_tokens":            provider.ParameterMaxOutputTokens,
	"max_completion_tokens": provider.ParameterMaxOutputTokens,
	"reasoning":             provider.ParameterReasoningEffort,
	"reasoning_effort":      provider.ParameterReasoningEffort,
	"response_format":       provider.ParameterResponseFormat,
	"frequency_penalty":     provider.ParameterFrequencyPenalty,
	"presence_penalty":      provider.ParameterPresencePenalty,
	"seed":                  provider.ParameterSeed,
	"stop":                  provider.ParameterStopSequences,
	"temperature":           provider.ParameterTemperature,
	"top_p":                 provider.ParameterTopP,
	"tool_choice":           provider.ParameterToolChoice,
	"tools":                 provider.ParameterTools,
}

// Health implements provider.Adapter.
//
// The probe is the provider's own model listing: it is the cheapest call that
// proves both reachability and that the configured credential is accepted,
// which are the two things a route decision turns on.
func (a *Adapter) Health(ctx context.Context) provider.Health {
	if a.Provider() == "typesafe" {
		return provider.Health{Provider: a.Provider(), CheckedAt: contract.NewTimestamp(time.Now()), Status: provider.HealthUnavailable, Detail: "decisions routes are dormant pending review"}
	}
	now := time.Now()
	pool := a.credentials.Projection(now)
	health := provider.Health{
		Provider:    a.config.Provider,
		CheckedAt:   contract.NewTimestamp(now),
		Credentials: &pool,
	}
	if !a.credentials.Configured() {
		// Distinct from unavailable on purpose: an operator reading
		// "unavailable" goes looking at the provider, and the answer is here.
		health.Status = provider.HealthUnconfigured
		health.Detail = "no credential is configured for this provider"
		return health
	}

	attempt := a.credentials.Begin()
	key, leased := attempt.NextProbe(now)
	if !leased {
		// Keys are declared and none of them can be used. That is a real
		// unavailability with a cause an operator can act on, and it is not
		// something a probe could discover — there is nothing to probe with.
		health.Status = provider.HealthUnavailable
		health.Detail = fmt.Sprintf("all %d credentials for this provider are out of rotation", pool.Declared)
		return health
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.config.BaseURL+"/models", nil)
	if err != nil {
		health.Status = provider.HealthUnavailable
		health.Detail = a.safeText(err.Error(), key)
		return health
	}
	a.authorize(request, key)

	startedAt := time.Now()
	response, err := a.client.Do(request)
	if err != nil {
		health.Status = provider.HealthUnavailable
		health.Detail = a.safeText(err.Error(), key)
		return health
	}
	defer func() { _ = response.Body.Close() }()

	latency := int(time.Since(startedAt).Milliseconds())
	health.LatencyMs = &latency
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		health.Status = provider.HealthOK
	case response.StatusCode == http.StatusTooManyRequests, response.StatusCode >= 500:
		health.Status = provider.HealthDegraded
		health.Detail = fmt.Sprintf("the provider answered %d", response.StatusCode)
	default:
		health.Status = provider.HealthUnavailable
		health.Detail = fmt.Sprintf("the provider answered %d", response.StatusCode)
	}
	if health.Status == provider.HealthOK && pool.Usable < pool.Declared {
		// The provider answered with the key it was probed with, and some of
		// the pool is spent or refused. Reporting that as plain `ok` would hide
		// a pool draining towards the moment it cannot serve at all, which is
		// the one thing an operator wants to see before a customer does.
		health.Status = provider.HealthDegraded
		health.Detail = fmt.Sprintf("%d of %d credentials for this provider are out of rotation",
			pool.Declared-pool.Usable, pool.Declared)
	}
	return health
}

// authorize applies one leased credential. It is the only method that touches
// a secret, and it is deliberately not part of Call: a translated call that
// carried a credential would be one struct away from a debug log.
func (a *Adapter) authorize(request *http.Request, key provider.Key) {
	request.Header.Set("Authorization", "Bearer "+key.Secret())
}

// Send implements provider.CredentialedSender: it performs the upstream call
// with one leased credential.
func (a *Adapter) Send(ctx context.Context, call *provider.Call, key provider.Key) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, call.Method, call.URL, bytes.NewReader(call.Body))
	if err != nil {
		return nil, fmt.Errorf("openaicompat: building the upstream request: %w", err)
	}
	request.Header = call.Header.Clone()
	a.authorize(request, key)
	if call.ScopedAttempt != nil {
		request.GetBody = nil
		request.Header.Del("Idempotency-Key")
		request.Header.Del("X-Idempotency-Key")
		request.Close = true
		client := provider.NewSingleAttemptHTTPClient()
		defer client.CloseIdleConnections()
		return client.Do(request)
	}
	return a.client.Do(request)
}

/* -------------------------------------------------------------------------- */
/*  Message translation                                                       */
/* -------------------------------------------------------------------------- */

func translateMessage(message contract.Message) (chatMessage, error) {
	translated := chatMessage{
		Role:       string(message.Role),
		Name:       message.Name,
		ToolCallID: message.ToolCallID,
	}
	for _, call := range message.ToolCalls {
		translated.ToolCalls = append(translated.ToolCalls, chatToolCall{
			ID:       call.ID,
			Type:     "function",
			Function: chatCallFunction{Name: call.Name, Arguments: call.Arguments},
		})
	}

	// A single text part becomes a plain string rather than a one-element
	// array. Several providers claiming OpenAI compatibility accept only the
	// string form for system and tool messages, and the array form is the one
	// that fails on them.
	if len(message.Content) == 1 && message.Content[0].Type == contract.ContentPartText {
		translated.Content = derefString(message.Content[0].Text)
		return translated, nil
	}

	parts := make([]any, 0, len(message.Content))
	for index, part := range message.Content {
		converted, err := translateContentPart(part)
		if err != nil {
			return chatMessage{}, annotate(err, fmt.Sprintf("content[%d]", index))
		}
		parts = append(parts, converted)
	}
	if len(parts) > 0 {
		translated.Content = parts
	}
	return translated, nil
}

func translateContentPart(part contract.ContentPart) (any, error) {
	switch part.Type {
	case contract.ContentPartText:
		return textPart{Type: "text", Text: derefString(part.Text)}, nil

	case contract.ContentPartImage:
		url, err := contentURL(part.Source)
		if err != nil {
			return nil, err
		}
		spec := imageURLSpec{URL: url}
		if part.Detail != nil {
			detail := string(*part.Detail)
			spec.Detail = &detail
		}
		return imagePart{Type: "image_url", ImageURL: spec}, nil

	case contract.ContentPartAudio:
		if part.Source.Kind != contract.ContentSourceInline {
			// The protocol takes audio as inline base64 only. Fetching the URL
			// here would make Kaana the one that downloads customer content,
			// which is a data-handling decision nobody has made.
			return nil, provider.ErrUnsupported{
				Code:   contract.CodeUnsupportedModality,
				Detail: "chat completions takes audio inline; a url source would require Kaana to fetch customer content",
			}
		}
		format, err := audioFormat(derefString(part.Source.MediaType))
		if err != nil {
			return nil, err
		}
		return audioPart{Type: "input_audio", InputAudio: inputAudioRef{
			Data:   derefString(part.Source.Data),
			Format: format,
		}}, nil

	case contract.ContentPartFile:
		if part.Source.Kind != contract.ContentSourceInline {
			return nil, provider.ErrUnsupported{
				Code:   contract.CodeUnsupportedModality,
				Detail: "chat completions takes files inline; a url source would require Kaana to fetch customer content",
			}
		}
		return filePart{Type: "file", File: fileRefSpec{
			FileData: dataURI(derefString(part.Source.MediaType), derefString(part.Source.Data)),
			Filename: part.Filename,
		}}, nil

	default:
		return nil, provider.ErrUnsupported{
			Code:   contract.CodeUnsupportedModality,
			Detail: fmt.Sprintf("chat completions has no representation for a %q part", part.Type),
		}
	}
}

func contentURL(source *contract.ContentSource) (string, error) {
	switch source.Kind {
	case contract.ContentSourceURL:
		return derefString(source.URL), nil
	case contract.ContentSourceInline:
		return dataURI(derefString(source.MediaType), derefString(source.Data)), nil
	default:
		return "", provider.ErrUnsupported{
			Code:   contract.CodeInvalidRequest,
			Detail: fmt.Sprintf("%q is not a content source kind", source.Kind),
		}
	}
}

func dataURI(mediaType, data string) string {
	return "data:" + mediaType + ";base64," + data
}

// audioFormat maps a media type to the protocol's short format name. The
// protocol takes a format rather than a media type, and an unrecognised one is
// refused rather than guessed: a wrong format is decoded as noise and billed as
// audio.
func audioFormat(mediaType string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0])) {
	case "audio/wav", "audio/wave", "audio/x-wav":
		return "wav", nil
	case "audio/mpeg", "audio/mp3":
		return "mp3", nil
	default:
		return "", provider.ErrUnsupported{
			Code:   contract.CodeUnsupportedModality,
			Detail: fmt.Sprintf("chat completions accepts wav or mp3 audio; %q is neither", mediaType),
		}
	}
}

func translateResponseFormat(format contract.ResponseFormat) (*responseFormat, error) {
	switch format.Type {
	case contract.ResponseFormatText:
		return nil, nil
	case contract.ResponseFormatJSONObject:
		return &responseFormat{Type: "json_object"}, nil
	case contract.ResponseFormatJSONSchema:
		if format.Name == nil || format.Schema == nil {
			return nil, provider.ErrUnsupported{
				Code:   contract.CodeInvalidRequest,
				Param:  "responseFormat",
				Detail: "a json_schema response format needs a name and a schema",
			}
		}
		return &responseFormat{
			Type: "json_schema",
			JSONSchema: &jsonSchemaSpec{
				Name:   *format.Name,
				Schema: format.Schema,
				Strict: format.Strict,
			},
		}, nil
	default:
		return nil, provider.ErrUnsupported{
			Code:   contract.CodeInvalidRequest,
			Param:  "responseFormat.type",
			Detail: fmt.Sprintf("%q is not a response format", format.Type),
		}
	}
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// annotate adds the request path to an unsupported-request refusal, so the
// customer is told which message or part could not be expressed rather than
// that "the request" could not be.
func annotate(err error, path string) error {
	var unsupported provider.ErrUnsupported
	// errors.As rather than a type assertion: a refusal that has been wrapped
	// on its way up would otherwise lose its path annotation and be reported to
	// the customer as "the request" rather than as the part at fault.
	if !errors.As(err, &unsupported) {
		return err
	}
	if unsupported.Param == "" {
		unsupported.Param = path
	} else {
		unsupported.Param = path + "." + unsupported.Param
	}
	return unsupported
}

// refuseUnstatedEffort refuses an effort the route's model does not take,
// where the adapter holds a reviewed per-model statement of the efforts it
// takes (providerconfig.ReasoningEfforts — today xAI's). xAI answers an effort
// its model does not document with an error after the request has crossed the
// network; the publisher publishes the same statement, so a request Oxy signed
// from the catalogue never reaches this refusal.
func refuseUnstatedEffort(slug contract.ProviderSlug, upstreamModelID string, effort contract.ReasoningEffort) error {
	accepted, stated := providerconfig.ReasoningEfforts(slug, upstreamModelID)
	if !stated {
		return nil
	}
	for _, candidate := range accepted {
		if candidate == effort {
			return nil
		}
	}
	detail := fmt.Sprintf("%s does not accept a reasoning effort for this model; omit reasoning to use the model's own behaviour", slug)
	if len(accepted) > 0 {
		names := make([]string, len(accepted))
		for index, candidate := range accepted {
			names[index] = string(candidate)
		}
		detail = fmt.Sprintf("%s accepts reasoning effort %s for this model, not %q", slug, strings.Join(names, ", "), effort)
	}
	return provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "reasoning.effort", Detail: detail}
}

type reasoningDialect int

const (
	reasoningUnreviewed reasoningDialect = iota
	// reasoningEffortField is Chat Completions' `reasoning_effort` string.
	reasoningEffortField
	// reasoningObject is OpenRouter's `reasoning: {"effort": ...}`.
	reasoningObject
)

// reasoningDialectFor names where a provider's own documentation puts the
// reasoning effort on a chat completion. It is a WIRE fact per provider, like
// OpenRouter's provider policy, not a model capability: which models take an
// effort at all is discovered from the providers' model lists and enforced by
// Oxy before the request is signed, and a model that still refuses the field
// is refused by its provider, never silently. The exception is a provider whose
// model list says nothing about efforts and whose API errors on one its model
// does not take: there the adapter states the per-model set itself
// (refuseUnstatedEffort) and refuses before sending.
//
//   - openai:     `reasoning_effort` — platform.openai.com/docs/api-reference/chat/create
//   - groq:       `reasoning_effort` — console.groq.com/docs/reasoning
//   - cerebras:   `reasoning_effort` — inference-docs.cerebras.ai/capabilities/reasoning
//   - xai:        `reasoning_effort` — docs.x.ai/docs/guides/reasoning
//   - openrouter: `reasoning.effort` — openrouter.ai/docs/use-cases/reasoning-tokens
//
// Every other slug is refused in Translate until its documentation is reviewed
// and a real-wire fake pins the field.
func reasoningDialectFor(slug contract.ProviderSlug) reasoningDialect {
	switch slug {
	case "openrouter":
		return reasoningObject
	case "openai", "groq", "cerebras", "xai":
		return reasoningEffortField
	default:
		return reasoningUnreviewed
	}
}
