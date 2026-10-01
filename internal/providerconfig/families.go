package providerconfig

import (
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// What a deployment can execute is decided per slug AND model, not per
// protocol. The protocol says which request families and session kinds an
// adapter can build at all (ExecutableAPIFormats, RealtimeSessionKinds,
// ChatOutputs); a model id, in the namespaces where the id names it, says
// which of those the model requires (ClassifyModel). Both halves live here
// because both commands read them: the publisher attaches a model only to a
// slug whose adapter can execute it, and the serving process refuses — in the
// executor, before Translate — a spoken request to a deployment that cannot
// answer aloud (SpeaksAloud).

// ChatOutput is what a slug's chat_completions path can produce: written text,
// spoken output (a request carrying `audioOutput`), or both.
type ChatOutput struct {
	Text   bool
	Spoken bool
}

// ChatOutputs is what a slug's adapter produces for chat_completions under a
// protocol. `openai_audio` answers ALOUD only and refuses a text chat; the
// OpenAI-compatible adapter writes text, and under `openrouter` — the one
// OpenAI-compatible slug whose gateway rows serve OpenAI's audio chat models —
// can also answer aloud (internal/provider/spokenchat). A protocol that
// executes no chat_completions produces neither.
func ChatOutputs(slug contract.ProviderSlug, protocol string) ChatOutput {
	switch protocol {
	case ProtocolOpenAIAudio:
		return ChatOutput{Spoken: true}
	case ProtocolOpenAICompatible:
		return ChatOutput{Text: true, Spoken: slug == "openrouter"}
	case ProtocolAnthropicMessages:
		return ChatOutput{Text: true}
	}
	return ChatOutput{}
}

// SpeaksAloud reports whether one deployment — a slug, under its protocol,
// serving one upstream model — answers a spoken chat request. The adapter must
// be able to speak AND the model must be one whose documented API answers
// aloud; a text model behind a speaking adapter is refused rather than sent a
// request its provider would turn down or, worse, answer in text.
func SpeaksAloud(slug contract.ProviderSlug, protocol, upstreamModelID string) bool {
	if !ChatOutputs(slug, protocol).Spoken {
		return false
	}
	required, classified, expressible := ClassifyModel(slug, upstreamModelID)
	return classified && expressible && required.Format == contract.APIFormatChatCompletions && required.Spoken
}

// ModelFamily is what one upstream model requires: a request family (and
// whether that family's output is SPOKEN), or a realtime session kind.
// Exactly one of Format and Session is set.
type ModelFamily struct {
	Format  contract.APIFormat
	Spoken  bool
	Session contract.RealtimeSessionKind
}

// ClassifyModel classifies a model id in the namespaces where the id alone
// names what the model's documented API requires:
//
//   - OpenAI's own slugs (`openai`, `openai-audio`, `openai-realtime`), and
//     OpenRouter's `openai/` rows, which are the same OpenAI models under the
//     gateway's namespace;
//   - xAI's own slugs (`xai`, `xai-realtime`);
//   - Groq's Llama Prompt Guard ids, and only those (groqClassifierOnly).
//
// classified is false everywhere else: the model list says nothing about the
// family there, and attribution alone decides. expressible is false when
// nothing the contract names can execute the model at all.
func ClassifyModel(slug contract.ProviderSlug, upstreamModelID string) (family ModelFamily, classified, expressible bool) {
	switch slug {
	case "typesafe":
		return ModelFamily{Format: contract.APIFormatDecisions}, true, false
	case "openai", "openai-audio", "openai-realtime":
		family, expressible = openAIFamily(upstreamModelID)
		return family, true, expressible
	case "openrouter":
		if strings.HasPrefix(strings.TrimPrefix(strings.ToLower(upstreamModelID), "~"), "typesafe/") {
			return ModelFamily{Format: contract.APIFormatDecisions}, true, false
		}
		if id, found := strings.CutPrefix(upstreamModelID, "openai/"); found {
			family, expressible = openAIFamily(id)
			return family, true, expressible
		}
	case "xai", "xai-realtime":
		return xAIFamily(upstreamModelID), true, true
	case "groq":
		if groqClassifierOnly(upstreamModelID) {
			return ModelFamily{}, true, false
		}
	}
	return ModelFamily{}, false, true
}

// groqClassifierOnly reports a Groq id that names a CLASSIFIER, not a
// generative model: Meta's Llama Prompt Guard 2 (22M and 86M) is an mDeBERTa
// sequence classifier with a 512-token window. Groq serves it at
// /chat/completions and answers with a label, but it cannot hold a
// conversation and fails a streamed chat request, and the contract names no
// classification family — so nothing Kaana executes can express it and it is
// never published (https://console.groq.com/docs/model/llama-prompt-guard-2-86m,
// .../llama-prompt-guard-2-22m, https://console.groq.com/docs/content-moderation).
//
// `openai/gpt-oss-safeguard-20b` is deliberately NOT here: Groq documents it as
// a generative reasoning model (tool use, JSON schema, reasoning effort) that
// classifies by writing text under a caller's policy, which is ordinary chat.
// Every other Groq id stays unclassified, so attribution alone decides it.
func groqClassifierOnly(upstreamModelID string) bool {
	return strings.HasPrefix(strings.ToLower(upstreamModelID), "meta-llama/llama-prompt-guard")
}

// openAIFamily classifies one of OpenAI's own model ids by what its
// documentation says it executes. The second return is false when nothing the
// contract names can express the model at all: GPT-Live is a separate session
// protocol (/v1/live/sessions), refused under every slug.
//
// Realtime ids name their session kind: gpt-realtime-translate translates,
// gpt-live-transcribe and gpt-realtime-whisper transcribe, and every other
// Realtime model holds conversations
// (https://developers.openai.com/api/docs/guides/realtime-translation,
// .../realtime-transcription). Whether a slug's adapter opens that kind is
// RealtimeSessionKinds' answer, not this function's.
//
// The audio chat models (`gpt-audio*`, `gpt-4o-audio-preview*`) are Chat
// Completions that answer aloud: chat_completions, spoken. That second bit is
// what keeps them off an adapter that executes chat_completions but cannot
// produce audio, and keeps a text model off the audio adapter, whose
// chat_completions path produces nothing else.
//
// It is conservative in one direction only: an id it does not recognise is
// classified as text chat, which is still attached only where the attribution
// table names it. Attribution stays the allow-list; this is the check that a
// reviewed id is attached to an adapter that can execute it.
func openAIFamily(upstreamModelID string) (ModelFamily, bool) {
	id := strings.ToLower(upstreamModelID)
	switch {
	case strings.HasPrefix(id, "gpt-live-transcribe"), strings.Contains(id, "realtime") && (strings.Contains(id, "whisper") || strings.Contains(id, "transcribe")):
		return ModelFamily{Session: contract.RealtimeTranscription}, true
	case strings.Contains(id, "realtime") && strings.Contains(id, "translate"):
		return ModelFamily{Session: contract.RealtimeTranslation}, true
	case strings.Contains(id, "realtime"):
		return ModelFamily{Session: contract.RealtimeConversation}, true
	case strings.HasPrefix(id, "gpt-live"):
		return ModelFamily{}, false
	case strings.Contains(id, "moderation"):
		return ModelFamily{}, false
	case strings.Contains(id, "transcribe"), strings.HasPrefix(id, "whisper"):
		return ModelFamily{Format: contract.APIFormatAudioTranscriptions}, true
	case strings.Contains(id, "tts"):
		return ModelFamily{Format: contract.APIFormatAudioSpeech}, true
	case strings.Contains(id, "audio"):
		return ModelFamily{Format: contract.APIFormatChatCompletions, Spoken: true}, true
	case strings.HasPrefix(id, "dall-e"), strings.HasPrefix(id, "gpt-image"), strings.HasPrefix(id, "sora"):
		return ModelFamily{Format: contract.APIFormatImagesGenerations}, true
	case strings.Contains(id, "embedding"):
		return ModelFamily{Format: contract.APIFormatEmbeddings}, true
	}
	return ModelFamily{Format: contract.APIFormatChatCompletions}, true
}

// xAIFamily classifies one of xAI's own ids. `grok-voice*` is the Voice Agent
// (speech-to-speech) API, a realtime conversation over wss://api.x.ai/v1/realtime
// (https://docs.x.ai/developers/model-capabilities/audio/speech-to-speech);
// `tts` is Kaana's name for xAI's model-less POST /v1/tts endpoint (the
// publisher's xAI speech discovery); every other id is text chat.
func xAIFamily(upstreamModelID string) ModelFamily {
	id := strings.ToLower(upstreamModelID)
	switch {
	case strings.HasPrefix(id, "grok-voice"):
		return ModelFamily{Session: contract.RealtimeConversation}
	case id == "tts":
		return ModelFamily{Format: contract.APIFormatAudioSpeech}
	}
	return ModelFamily{Format: contract.APIFormatChatCompletions}
}
