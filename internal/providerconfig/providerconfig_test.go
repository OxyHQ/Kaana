package providerconfig_test

import (
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

func TestEnvironmentPrefixUsesTheKaanaName(t *testing.T) {
	if got := providerconfig.EnvironmentPrefix("openrouter"); got != "KAANA_PROVIDER_OPENROUTER" {
		t.Fatalf("EnvironmentPrefix = %q", got)
	}
	if got := providerconfig.EnvironmentPrefix("x-ai"); got != "KAANA_PROVIDER_X_AI" {
		t.Fatalf("EnvironmentPrefix folded the separator wrongly: %q", got)
	}
}

func TestVerifiedProviderEndpointsAreBuiltIn(t *testing.T) {
	if got := len(providerconfig.Known); got != 24 {
		t.Fatalf("built-in providers = %d, want the 24 documented in README.md and docs/operating.md", got)
	}
	want := map[contract.ProviderSlug]string{
		"mistral":      "https://api.mistral.ai/v1",
		"deepseek":     "https://api.deepseek.com",
		"sambanova":    "https://api.sambanova.ai/v1",
		"siliconflow":  "https://api.siliconflow.cn/v1",
		"ai21":         "https://api.ai21.com/studio/v1",
		"google":       "https://generativelanguage.googleapis.com/v1beta/openai",
		"together":     "https://api.together.ai/v1",
		"cohere":       "https://api.cohere.ai/compatibility/v1",
		"fireworks":    "https://api.fireworks.ai/inference/v1",
		"hyperbolic":   "https://api.hyperbolic.xyz/v1",
		"digitalocean": "https://inference.do-ai.run/v1",
		"nvidia":       "https://integrate.api.nvidia.com/v1",
		"modelscope":   "https://api-inference.modelscope.cn/v1",
		"zai":          "https://open.bigmodel.cn/api/paas/v4",
		"nebius":       "https://api.tokenfactory.nebius.com/v1",
		"nscale":       "https://inference.api.nscale.com/v1",
		"chutes":       "https://llm.chutes.ai/v1",
		"ovhcloud":     "https://oai.endpoints.kepler.ai.cloud.ovh.net/v1",
	}
	for slug, baseURL := range want {
		endpoint, ok := providerconfig.Known[slug]
		if !ok || endpoint.Protocol != providerconfig.ProtocolOpenAICompatible || endpoint.BaseURL != baseURL {
			t.Errorf("%s endpoint = %+v", slug, endpoint)
		}
	}
	if providerconfig.Known["ai21"].Discovery != providerconfig.DiscoveryNotAvailable {
		t.Fatal("AI21 was assigned a model-list endpoint its official API does not publish")
	}
	for _, slug := range []contract.ProviderSlug{"google", "cohere", "fireworks", "hyperbolic", "nvidia", "modelscope", "zai", "chutes", "ovhcloud"} {
		if providerconfig.Known[slug].Discovery != providerconfig.DiscoveryNotAvailable {
			t.Errorf("%s was assigned generic discovery without a documented OpenAI-shaped account list at its compatibility base", slug)
		}
	}
	if providerconfig.Known["nebius"].Discovery != providerconfig.DiscoveryNebiusModels {
		t.Error("Nebius lost the discovery profile that rejects delivery flavours")
	}
	if providerconfig.Known["nscale"].Discovery != providerconfig.DiscoveryOpenAIModels {
		t.Error("Nscale lost its documented authenticated OpenAI model-list contract")
	}
}

func TestProviderBaseURLMustBeVerifiedHTTPS(t *testing.T) {
	for _, raw := range []string{
		"http://api.example.invalid/v1",
		"https://user:secret@api.example.invalid/v1",
		"https://api.example.invalid/v1?key=secret",
		"//api.example.invalid/v1",
		"https://sk-abcdefghijkl.api.example.invalid/v1",
		"https://api.example.invalid/v1/sk-abcdefghijkl",
		"https://api.example.invalid/v1?api_key=abcdefgh",
		"https://api_key=abcdefgh@api.example.invalid/v1",
	} {
		if err := providerconfig.ValidateBaseURL(raw); err == nil {
			t.Errorf("ValidateBaseURL(%q) accepted an unsafe URL", raw)
		}
	}
	for _, raw := range []string{
		"https://api.example.invalid/v1",
		"https://dashscope-intl.aliyuncs.com/api/v1/workspaces/ws_1234567890/inference",
	} {
		if err := providerconfig.ValidateBaseURL(raw); err != nil {
			t.Fatalf("valid public provider URL %q was refused: %v", raw, err)
		}
	}
}
