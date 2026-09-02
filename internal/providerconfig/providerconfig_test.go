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
	if got := len(providerconfig.Known); got != 26 {
		t.Fatalf("built-in providers = %d, want the 26 documented in README.md and docs/operating.md", got)
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
	cerebras := providerconfig.Known["cerebras"]
	if cerebras.Protocol != providerconfig.ProtocolOpenAICompatible ||
		cerebras.BaseURL != "https://api.cerebras.ai/v1" ||
		cerebras.Discovery != providerconfig.DiscoveryOpenAIModels {
		t.Fatalf("Cerebras serving/discovery contract = %+v", cerebras)
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
	if endpoint := providerconfig.Known["alibaba"]; endpoint.Protocol != providerconfig.ProtocolOpenAICompatible || endpoint.BaseURL != "" || endpoint.Discovery != providerconfig.DiscoveryAlibabaModels {
		t.Errorf("Alibaba dynamic endpoint = %+v", endpoint)
	}
	if endpoint := providerconfig.Known["cloudflare"]; endpoint.Protocol != providerconfig.ProtocolOpenAICompatible || endpoint.BaseURL != "" || endpoint.Discovery != providerconfig.DiscoveryNotAvailable {
		t.Errorf("Cloudflare dynamic endpoint = %+v", endpoint)
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

func TestOpenRouterEndpointIdentityCannotBeAliasedOrBorrowed(t *testing.T) {
	if raw := "https://openrouter.ai/api/v1"; providerconfig.ValidateEndpointIdentity("openrouter", raw) != nil {
		t.Errorf("canonical OpenRouter endpoint %q was refused", raw)
	}

	for _, raw := range []string{
		"http://openrouter.ai/api/v1",
		"https://openrouter.ai:443/api/v1",
		"https://openrouter.ai:8443/api/v1",
		"https://openrouter.ai/v1",
		"https://openrouter.ai/api/v1/",
		"HTTPS://OPENROUTER.AI./api/./v1/",
		"https://www.openrouter.ai/api/v1",
		"https://api.openrouter.ai/api/v1",
		"https://openrouter.ai/api/other/../v1",
		"https://openrouter.ai/api//v1",
		"https://openrouter.ai/api/%76%31",
		"https://user@openrouter.ai/api/v1",
		"https://openrouter.ai/api/v1?route=other",
		"https://openrouter.ai/api/v1#other",
		"https://notopenrouter.ai/api/v1",
	} {
		if err := providerconfig.ValidateEndpointIdentity("openrouter", raw); err == nil {
			t.Errorf("OpenRouter accepted non-canonical endpoint %q", raw)
		}
	}

	for _, raw := range []string{
		"https://openrouter.ai/api/v1",
		"https://OPENROUTER.AI.:443/api/./v1/",
		"https://user@openrouter.ai/api/v1",
		"https://www.openrouter.ai/another/path",
		"https://api.openrouter.ai:8443/api/v2",
	} {
		if err := providerconfig.ValidateEndpointIdentity("custom-compatible", raw); err == nil {
			t.Errorf("another slug borrowed reserved OpenRouter endpoint %q", raw)
		}
	}

	for _, raw := range []string{
		"https://openrouter.ai.example.com/api/v1",
		"https://notopenrouter.ai/api/v1",
		"https://openrouter-api.ai/api/v1",
		"https://example.com/openrouter.ai/api/v1",
		"https://openrouter.ai@evil.example/api/v1",
		"https://user@notopenrouter.ai/api/v1",
	} {
		if err := providerconfig.ValidateEndpointIdentity("custom-compatible", raw); err != nil {
			t.Errorf("similar but unrelated endpoint %q was reserved: %v", raw, err)
		}
	}
}

func TestAccountScopedProviderEndpointIdentityIsExact(t *testing.T) {
	accepted := map[contract.ProviderSlug][]string{
		"alibaba": {
			"https://dashscope.aliyuncs.com/compatible-mode/v1",
			"https://dashscope-intl.aliyuncs.com/compatible-mode/v1",
			"https://dashscope-us.aliyuncs.com/compatible-mode/v1",
			"https://workspace-opaque.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1",
			"https://workspace-opaque.eu-central-1.maas.aliyuncs.com/compatible-mode/v1",
		},
		"cloudflare": {
			"https://api.cloudflare.com/client/v4/accounts/account-opaque/ai/v1",
		},
	}
	for slug, addresses := range accepted {
		for _, raw := range addresses {
			if err := providerconfig.ValidateEndpointIdentity(slug, raw); err != nil {
				t.Errorf("%s endpoint %q was refused: %v", slug, raw, err)
			}
		}
	}

	for _, candidate := range []struct {
		slug contract.ProviderSlug
		raw  string
	}{
		{slug: "alibaba", raw: "https://dashscope-intl.aliyuncs.com/api/v1"},
		{slug: "alibaba", raw: "https://workspace-opaque.example.com/compatible-mode/v1"},
		{slug: "alibaba", raw: "https://workspace-opaque.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1/"},
		{slug: "alibaba", raw: "http://dashscope-intl.aliyuncs.com/compatible-mode/v1"},
		{slug: "cloudflare", raw: "https://api.cloudflare.com/client/v4/accounts/account-opaque/ai/v1/"},
		{slug: "cloudflare", raw: "https://api.cloudflare.com/client/v4/accounts//ai/v1"},
		{slug: "cloudflare", raw: "https://api.cloudflare.com/client/v4/accounts/../ai/v1"},
		{slug: "cloudflare", raw: "https://api.cloudflare.com/client/v4/accounts/account-opaque/ai/v1?route=other"},
		{slug: "cloudflare", raw: "https://example.com/client/v4/accounts/account-opaque/ai/v1"},
		{slug: "custom-compatible", raw: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"},
		{slug: "custom-compatible", raw: "https://api.cloudflare.com/client/v4/accounts/account-opaque/ai/v1"},
	} {
		if err := providerconfig.ValidateEndpointIdentity(candidate.slug, candidate.raw); err == nil {
			t.Errorf("provider %q accepted mismatched endpoint %q", candidate.slug, candidate.raw)
		}
	}
}
