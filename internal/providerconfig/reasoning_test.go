package providerconfig

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// Every xAI chat id the checked-in attribution publishes has a reviewed row,
// and the table holds nothing else: a newly attributed id must be read against
// xAI's model page here rather than silently taking no effort.
func TestEveryAttributedXAIChatModelHasAReviewedEffortStatement(t *testing.T) {
	raw, err := os.ReadFile("../../configs/model-attribution.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Attribution map[contract.ProviderSlug]map[string]string `json:"attribution"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	attributed := 0
	for id := range file.Attribution["xai"] {
		if xAIFamily(id).Format != contract.APIFormatChatCompletions {
			continue
		}
		attributed++
		if _, reviewed := xAIReasoningEfforts[id]; !reviewed {
			t.Errorf("xai/%s is attributed and has no reviewed reasoning-effort row", id)
		}
	}
	const want = 8
	if attributed != want || len(xAIReasoningEfforts) != want {
		t.Errorf("%d attributed xAI chat ids and %d reviewed rows, want exactly %d of each", attributed, len(xAIReasoningEfforts), want)
	}
}

func TestReasoningEffortsAreStatedOnlyForXAIChat(t *testing.T) {
	all := contract.ReasoningEfforts()
	for _, c := range []struct {
		slug   contract.ProviderSlug
		id     string
		want   []contract.ReasoningEffort
		stated bool
	}{
		{"xai", "grok-4.7", all, true},
		{"xai", "grok-4.3", all, true},
		{"xai", "grok-build-0.1", []contract.ReasoningEffort{}, true},
		{"xai", "grok-4.20-0309-reasoning", []contract.ReasoningEffort{}, true},
		{"xai", "grok-4.20-multi-agent-0309", []contract.ReasoningEffort{}, true},
		{"xai", "grok-9-unreviewed", []contract.ReasoningEffort{}, true},
		{"xai", "tts", nil, false},
		{"xai-realtime", "grok-voice-think-fast-2.0", nil, false},
		{"openrouter", "x-ai/grok-build-0.1", nil, false},
		{"groq", "openai/gpt-oss-120b", nil, false},
	} {
		got, stated := ReasoningEfforts(c.slug, c.id)
		if stated != c.stated || (stated && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("%s/%s = %v, %t; want %v, %t", c.slug, c.id, got, stated, c.want, c.stated)
		}
	}
	// The returned list is a copy: a caller cannot edit the reviewed table.
	got, _ := ReasoningEfforts("xai", "grok-4.7")
	got[0] = contract.ReasoningEffortHigh
	if again, _ := ReasoningEfforts("xai", "grok-4.7"); again[0] != contract.ReasoningEffortLow {
		t.Error("ReasoningEfforts returned the table's own slice")
	}
}

func TestGroqPromptGuardIsAClassifierNothingExecutes(t *testing.T) {
	for _, id := range []string{"meta-llama/llama-prompt-guard-2-22m", "meta-llama/llama-prompt-guard-2-86m"} {
		if _, classified, expressible := ClassifyModel("groq", id); !classified || expressible {
			t.Errorf("groq/%s = classified %t, expressible %t; want an inexpressible classifier", id, classified, expressible)
		}
	}
	// Controls: a generative safety model and an ordinary chat model on Groq,
	// and Whisper (a transcription model the chat classification must not
	// reach), are left to attribution.
	for _, id := range []string{"openai/gpt-oss-safeguard-20b", "openai/gpt-oss-120b", "whisper-large-v3"} {
		if _, classified, _ := ClassifyModel("groq", id); classified {
			t.Errorf("groq/%s is classified; only Prompt Guard is", id)
		}
	}
}
