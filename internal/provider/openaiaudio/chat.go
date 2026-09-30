package openaiaudio

import (
	"context"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/spokenchat"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// Audio chat: a conversational model answering ALOUD over Chat Completions
// (`gpt-audio-1.5`), requested with the contract's `audioOutput`. The wire, the
// reader and the audio-token partition are internal/provider/spokenchat, which
// OpenRouter's gateway rows for the same models share. What is this adapter's
// own is the origin, and that its chat_completions path answers aloud ONLY: the
// publisher attaches a text chat model to `openai` and never here, and this
// path refuses every chat request that does not ask to be answered aloud, so a
// text chat can never be billed at audio rates.

// openAIDialect is OpenAI's own spoken chat: whole answers are documented, and
// a streamed one is pcm16.
var openAIDialect = spokenchat.Dialect{Provider: Slug, Name: "OpenAI"}

// ChatOutputs implements provider.ChatOutputDeclarer: this adapter never
// writes a text chat, and speaks only for a model that answers aloud (not,
// say, a transcription deployment handed a chat request).
func (a *Adapter) ChatOutputs(route provider.Route) providerconfig.ChatOutput {
	return providerconfig.ChatOutput{Spoken: providerconfig.SpeaksAloud(Slug, providerconfig.ProtocolOpenAIAudio, route.UpstreamModelID)}
}

func (a *Adapter) translateChat(r *contract.Request, route provider.Route) (*provider.Call, error) {
	return spokenchat.Translate(r, route, a.base+"/chat/completions", openAIDialect, nil)
}

func (a *Adapter) streamChat(ctx context.Context, call *provider.Call, out provider.Emitter, credentials *provider.KeyPool) (provider.Outcome, error) {
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
	measured, err := spokenchat.Read(ctx, response.Body, call, out, key, openAIDialect, a)
	measured.KeyID, measured.KeyClass = key.ID, key.Class
	return measured, err
}

// StreamFailure implements spokenchat.Failures: an error object that arrived
// inside the stream, after a 200. There is no status, so OpenAI's own error
// type and code are all there is; an unrecognised one is an unattributed
// provider failure.
func (a *Adapter) StreamFailure(reported spokenchat.StreamError, key provider.Key) error {
	failure := provider.ErrUpstream{Passthrough: &contract.ProviderErrorPassthrough{Provider: Slug}}
	if reported.Type != "" {
		kind := contract.SafeErrorText(provider.RedactSecret(reported.Type, key.Secret()))
		failure.Passthrough.Code = &kind
	}
	if reported.Message != "" {
		message := contract.SafeErrorText(provider.RedactSecret(reported.Message, key.Secret()))
		failure.Passthrough.Message = &message
	}
	code := reported.CodeString()
	switch {
	case reported.Type == "insufficient_quota" || code == "insufficient_quota":
		failure.Code, failure.Category = contract.CodeProviderBillingRefused, contract.UpstreamQuota
		failure.Detail = "the platform's own OpenAI account cannot be billed for this request"
	case code == "invalid_api_key" || reported.Type == "authentication_error":
		failure.Code, failure.Category = contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication
		failure.Detail = "OpenAI refused the platform's credential for this route"
	case code == "rate_limit_exceeded" || reported.Type == "rate_limit_error":
		failure.Code, failure.Category = contract.CodeRateLimited, contract.UpstreamRateLimit
		failure.Detail = "OpenAI rate-limited this request part-way through it"
	case reported.Type == "server_error":
		failure.Code, failure.Category = contract.CodeProviderError, contract.UpstreamServerError
		failure.Detail = "OpenAI failed part-way through the response"
	default:
		failure.Code, failure.Category = contract.CodeProviderError, contract.UpstreamUnknown
		failure.Detail = "OpenAI failed part-way through the response and named no reason this build knows"
	}
	return provider.CustomerCredentialFailure(key, failure)
}
