package openaicompat

import (
	"context"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/spokenchat"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// Spoken chat through OpenRouter: its rows for OpenAI's audio chat models
// (`openai/gpt-audio`, `openai/gpt-audio-mini`) answer aloud over the same
// Chat Completions wire OpenAI documents, and OpenRouter documents the same
// request (`modalities: ["text","audio"]`, `audio: {voice, format}`), the same
// `delta.audio {data, transcript}` stream and the same usage nesting
// (https://openrouter.ai/docs/guides/overview/multimodal/audio,
// https://openrouter.ai/docs/api/api-reference/chat/create-a-chat-completion).
// The wire is therefore internal/provider/spokenchat, shared with the
// `openai-audio` adapter, and what is OpenRouter's own is:
//
//   - it streams spoken output only ("Audio output requires streaming"), so the
//     upstream is always streamed and a whole answer is the same events folded;
//   - Kaana's mandatory provider policy rides on the body exactly as it does on
//     a text request;
//   - its in-stream errors are classified by this adapter's vocabulary.
//
// Which deployments speak is providerconfig.SpeaksAloud: the `openrouter` slug
// AND a model OpenAI documents as answering aloud. A text model on OpenRouter,
// and every other OpenAI-compatible slug, is refused before anything is sent.

func (a *Adapter) spokenDialect() spokenchat.Dialect {
	return spokenchat.Dialect{Provider: a.config.Provider, Name: string(a.config.Provider), StreamOnly: a.config.Provider == "openrouter"}
}

// ChatOutputs implements provider.ChatOutputDeclarer: text on every
// deployment, and spoken output only where the slug and the model both speak.
func (a *Adapter) ChatOutputs(route provider.Route) providerconfig.ChatOutput {
	outputs := providerconfig.ChatOutputs(a.config.Provider, providerconfig.ProtocolOpenAICompatible)
	outputs.Spoken = providerconfig.SpeaksAloud(a.config.Provider, providerconfig.ProtocolOpenAICompatible, route.UpstreamModelID)
	return outputs
}

func (a *Adapter) translateSpoken(request *contract.Request, route provider.Route) (*provider.Call, error) {
	var policy any
	if a.config.Provider == "openrouter" {
		// The same non-negotiable controls a text request carries.
		policy = &openRouterProviderPolicy{ZDR: true, DataCollection: "deny", RequireParameters: true}
	}
	call, err := spokenchat.Translate(request, route, a.config.BaseURL+"/chat/completions", a.spokenDialect(), policy)
	if err != nil {
		return nil, err
	}
	for name, value := range a.config.Headers {
		call.Header.Set(name, value)
	}
	return call, nil
}

func (a *Adapter) streamSpoken(ctx context.Context, call *provider.Call, out provider.Emitter, credentials *provider.KeyPool) (provider.Outcome, error) {
	outcome := provider.Outcome{UsageSource: contract.UsageEstimated}
	if credentials == nil {
		credentials = a.credentials
	}
	response, key, err := provider.Walk(ctx, credentials, call, a)
	outcome.KeyID, outcome.KeyClass = key.ID, key.Class
	if err != nil {
		return outcome, err
	}
	defer func() { _ = response.Body.Close() }()
	measured, err := spokenchat.Read(ctx, response.Body, call, out, key, a.spokenDialect(), a)
	measured.KeyID, measured.KeyClass = key.ID, key.Class
	return measured, err
}

// StreamFailure implements spokenchat.Failures with this adapter's own
// in-stream vocabulary.
func (a *Adapter) StreamFailure(reported spokenchat.StreamError, key provider.Key) error {
	return a.streamFailure(upstreamError{Message: reported.Message, Type: reported.Type, Code: reported.Code}, key)
}
