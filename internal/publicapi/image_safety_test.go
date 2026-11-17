package publicapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
	"github.com/OxyHQ/Kaana/internal/publicapi"
)

// scriptedImageSafety is Kaana's content check as a test scripts it. It
// records what it was asked and, through authorizations, how many grants had
// been asked for when the prompt question arrived.
type scriptedImageSafety struct {
	mu             sync.Mutex
	prompt, output publicapi.ImageSafetyVerdict
	promptErr      error
	outputErr      error
	authorizations func() int

	prompts        []string
	grantsAtPrompt []int
	outputs        [][]provider.GeneratedImage
}

func allowingImageSafety() *scriptedImageSafety {
	return &scriptedImageSafety{prompt: publicapi.ImageSafetyAllowed, output: publicapi.ImageSafetyAllowed}
}

func (s *scriptedImageSafety) CheckPrompt(_ context.Context, prompt string) (publicapi.ImageSafetyVerdict, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = append(s.prompts, prompt)
	if s.authorizations != nil {
		s.grantsAtPrompt = append(s.grantsAtPrompt, s.authorizations())
	}
	return s.prompt, s.promptErr
}

func (s *scriptedImageSafety) CheckImages(_ context.Context, _ string, images []provider.GeneratedImage) (publicapi.ImageSafetyVerdict, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outputs = append(s.outputs, slices.Clone(images))
	return s.output, s.outputErr
}

func (s *scriptedImageSafety) script(change func(*scriptedImageSafety)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(s)
}

func (s *scriptedImageSafety) asked() (prompts, outputs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prompts), len(s.outputs)
}

// imageCatalogue declares the stub image line with the given safety member
// (nil declares none) beside the ordinary declared models.
func imageCatalogue(t *testing.T, safety map[string]any) *publicapi.PublicCatalogue {
	t.Helper()
	catalogue, err := publicapi.ParsePublicCatalogue(imageCatalogueFile(t, safety))
	if err != nil {
		t.Fatal(err)
	}
	return catalogue
}

func imageCatalogueFile(t *testing.T, safety map[string]any) []byte {
	t.Helper()
	var file struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal([]byte(declaredCatalogue), &file); err != nil {
		t.Fatal(err)
	}
	line := map[string]any{
		"modelId": string(imageModel.ModelID()), "publisher": map[string]any{"slug": "stubimage", "displayName": "Stub Images"}, "displayName": "Stub Draw",
		"capabilities": map[string]any{"inputModalities": []any{"text"}, "outputModalities": []any{"image"}, "tools": false, "parallelToolCalls": false,
			"structuredOutput": false, "jsonMode": false, "reasoning": false, "streaming": false, "promptCaching": false, "maxContextTokens": 32000, "maxOutputTokens": 1},
		"license":     map[string]any{"licenseId": "proprietary", "displayName": "Proprietary", "commercialUseAllowed": true, "requiresAttribution": false},
		"provenance":  map[string]any{"releaseKind": "third_party_hosted"},
		"dataPolicy":  map[string]any{"retainsPayloads": false, "retentionDays": 0, "trainsOnCustomerData": false, "zeroDataRetentionAvailable": true},
		"deprecation": map[string]any{"status": "active"},
	}
	if safety != nil {
		line["safety"] = safety
	}
	raw, err := json.Marshal(map[string]any{"schemaVersion": 1, "models": append(file.Models, line)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// safeImageLine is the declaration image generation is served on.
var safeImageLine = map[string]any{"contentFilteringDefault": "strict", "knownLimitations": []any{}, "provenanceMarking": "c2pa"}

// newImageHarness serves the stub image line: its catalogue declares strict
// filtering and a provenance marking, and the content check allows everything
// until a test scripts it otherwise.
func newImageHarness(t *testing.T) (*harness, *scriptedImageSafety) {
	t.Helper()
	safety := allowingImageSafety()
	h := newHarness(t, harnessOptions{catalogue: imageCatalogue(t, safeImageLine), imageSafety: safety})
	safety.script(func(s *scriptedImageSafety) {
		s.authorizations = func() int { return len(h.oxy.authorizationBodies()) }
	})
	return h, safety
}

// The prompt is asked about after validation and before Oxy: when the
// question arrives no grant has been asked for, and the check sees the exact
// prompt. The images are asked about before the caller receives them.
func TestTheImageContentCheckRunsBeforeTheGrantAndBeforeTheAnswer(t *testing.T) {
	h, safety := newImageHarness(t)
	response := h.post("/v1/images/generations", validKey, imageBody(map[string]any{"n": 2}))
	if body := readAll(t, response); response.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	if len(safety.prompts) != 1 || safety.prompts[0] != "A SECRET-PROMPT fox" {
		t.Fatalf("the check was asked about %d prompts", len(safety.prompts))
	}
	if len(safety.grantsAtPrompt) != 1 || safety.grantsAtPrompt[0] != 0 {
		t.Errorf("grants asked for when the prompt was checked = %v; want none", safety.grantsAtPrompt)
	}
	if len(safety.outputs) != 1 || len(safety.outputs[0]) != 2 || safety.outputs[0][0].B64JSON != "SYNTHETIC-IMAGE-A" {
		t.Errorf("the check was asked about %d answers", len(safety.outputs))
	}
	if len(h.oxy.authorizationBodies()) != 1 {
		t.Errorf("%d authorizations for one served request", len(h.oxy.authorizationBodies()))
	}

	// An invalid request never reaches the check: validation comes first.
	publicError(t, h.post("/v1/images/generations", validKey, imageBody(map[string]any{"n": 11})), http.StatusBadRequest)
	if prompts, _ := safety.asked(); prompts != 1 {
		t.Errorf("an invalid request was checked: %d prompt questions", prompts)
	}
}

// A prompt the check refuses, and a prompt the check cannot answer about, are
// both refused before any grant: nothing is held and no provider is called.
// The second is the fail-closed half: an error, and an answer that is neither
// allowed nor refused, serve nothing.
func TestAPromptTheContentCheckDoesNotAllowHoldsNothing(t *testing.T) {
	for name, scenario := range map[string]struct {
		script func(*scriptedImageSafety)
		status int
		code   contract.PublicErrorCode
		retry  bool
		param  string
	}{
		"refused":    {func(s *scriptedImageSafety) { s.prompt = publicapi.ImageSafetyRefused }, http.StatusForbidden, "policy_violation", false, "prompt"},
		"an error":   {func(s *scriptedImageSafety) { s.promptErr = errors.New("classifier unreachable: A SECRET-PROMPT fox") }, http.StatusServiceUnavailable, "service_unavailable", true, ""},
		"no answer":  {func(s *scriptedImageSafety) { s.prompt = publicapi.ImageSafetyUnanswered }, http.StatusServiceUnavailable, "service_unavailable", true, ""},
		"an unknown": {func(s *scriptedImageSafety) { s.prompt = publicapi.ImageSafetyVerdict(99) }, http.StatusServiceUnavailable, "service_unavailable", true, ""},
		"an allowance beside an error": {func(s *scriptedImageSafety) {
			s.prompt, s.promptErr = publicapi.ImageSafetyAllowed, errors.New("partial answer")
		}, http.StatusServiceUnavailable, "service_unavailable", true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			h, safety := newImageHarness(t)
			safety.script(scenario.script)
			response := h.post("/v1/images/generations", validKey, imageBody(nil))
			failure := publicError(t, response, scenario.status)
			if failure.Code != scenario.code || failure.Retryable != scenario.retry {
				t.Errorf("failure = %+v", failure)
			}
			if got := value(failure.Param); got != scenario.param {
				t.Errorf("param = %q, want %q", got, scenario.param)
			}
			if strings.Contains(string(failure.Message), "SECRET-PROMPT") || strings.Contains(string(failure.Message), "classifier unreachable") {
				t.Errorf("the refusal quotes the prompt or the check's error: %s", failure.Message)
			}
			if len(h.oxy.authorizationBodies()) != 0 || len(h.image.called()) != 0 {
				t.Errorf("%d authorizations, %d adapter calls; a prompt that was not allowed holds nothing", len(h.oxy.authorizationBodies()), len(h.image.called()))
			}
			if _, outputs := safety.asked(); outputs != 0 {
				t.Error("the output question was asked about a request that generated nothing")
			}
			if strings.Contains(h.logs.String(), "SECRET-PROMPT") || strings.Contains(h.logs.String(), "classifier unreachable") {
				t.Error("the prompt or the check's error reached the logs")
			}

			// The control: the same harness serves the same request once the
			// check allows it, so the refusal above was the check's answer.
			safety.script(func(s *scriptedImageSafety) { s.prompt, s.promptErr = publicapi.ImageSafetyAllowed, nil })
			if served := h.post("/v1/images/generations", validKey, imageBody(nil)); served.StatusCode != http.StatusOK {
				t.Fatalf("the allowed prompt was refused: %d", served.StatusCode)
			}
		})
	}
}

// Images the check refuses, or cannot answer about, never reach the caller:
// the answer carries no image. The provider drew them, so the grant settles
// what was generated.
func TestGeneratedImagesTheContentCheckDoesNotAllowAreWithheld(t *testing.T) {
	for name, scenario := range map[string]struct {
		script func(*scriptedImageSafety)
		status int
		code   contract.PublicErrorCode
	}{
		"refused":    {func(s *scriptedImageSafety) { s.output = publicapi.ImageSafetyRefused }, http.StatusForbidden, "policy_violation"},
		"an error":   {func(s *scriptedImageSafety) { s.outputErr = errors.New("classifier unreachable") }, http.StatusServiceUnavailable, "service_unavailable"},
		"no answer":  {func(s *scriptedImageSafety) { s.output = publicapi.ImageSafetyUnanswered }, http.StatusServiceUnavailable, "service_unavailable"},
		"an unknown": {func(s *scriptedImageSafety) { s.output = publicapi.ImageSafetyVerdict(99) }, http.StatusServiceUnavailable, "service_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			h, safety := newImageHarness(t)
			safety.script(scenario.script)
			response := h.post("/v1/images/generations", validKey, imageBody(map[string]any{"n": 2}))
			body := readAll(t, response)
			if response.StatusCode != scenario.status {
				t.Fatalf("status %d, want %d: %s", response.StatusCode, scenario.status, body)
			}
			var failure contract.PublicError
			if err := json.Unmarshal(body, &failure); err != nil || failure.Code != scenario.code || failure.Param != nil {
				t.Errorf("failure = %s", body)
			}
			if strings.Contains(string(body), "SYNTHETIC-IMAGE") || strings.Contains(string(body), "b64_json") || strings.Contains(string(body), "classifier unreachable") {
				t.Errorf("a withheld answer carries an image or the check's error: %s", body)
			}
			if len(h.image.called()) != 1 {
				t.Fatalf("%d adapter calls; the output question follows one generation", len(h.image.called()))
			}
			h.settle()
			settled := h.oxy.settled()
			if len(settled) != 1 {
				t.Fatalf("%d settlements; a generated answer settles once even when withheld", len(settled))
			}
			for _, settlement := range settled {
				units := map[contract.UsageUnit]int{}
				for _, unit := range settlement.Report.Units {
					units[unit.Unit] = unit.Quantity
				}
				if units[contract.UnitImages] != 2 {
					t.Errorf("settled units = %+v; the provider drew two images", settlement.Report.Units)
				}
			}
			if strings.Contains(h.logs.String(), "SYNTHETIC-IMAGE") || strings.Contains(h.logs.String(), "classifier unreachable") {
				t.Error("a generated image or the check's error reached the logs")
			}

			// The control: allowed, the same harness returns the images.
			safety.script(func(s *scriptedImageSafety) { s.output, s.outputErr = publicapi.ImageSafetyAllowed, nil })
			served := h.post("/v1/images/generations", validKey, imageBody(nil))
			if answer := readAll(t, served); served.StatusCode != http.StatusOK || !strings.Contains(string(answer), "SYNTHETIC-IMAGE-A") {
				t.Fatalf("the allowed images were withheld: %d %s", served.StatusCode, answer)
			}
		})
	}
}

// The check answers nobody who presents no credential: the request is refused
// as unauthenticated before the prompt question is asked.
func TestTheImageContentCheckIsNotAskedWithoutACredential(t *testing.T) {
	h, safety := newImageHarness(t)
	failure := publicError(t, h.post("/v1/images/generations", "", imageBody(nil)), http.StatusUnauthorized)
	if failure.Code != "authentication_failed" {
		t.Errorf("failure = %+v", failure)
	}
	if prompts, _ := safety.asked(); prompts != 0 {
		t.Errorf("the check was asked %d prompt questions by a caller with no credential", prompts)
	}
	if served := h.post("/v1/images/generations", validKey, imageBody(nil)); served.StatusCode != http.StatusOK {
		t.Fatalf("the credentialed request was refused: %d", served.StatusCode)
	}
}

// Image generation is served only on a line whose declaration states strict
// content filtering and a provenance marking, and only by a process that has
// the content check. Everything weaker is refused before any grant, although
// the same deployment could draw the image.
func TestImageGenerationIsServedOnlyOnADeclaredSafeLine(t *testing.T) {
	declared := func(filtering, marking string) map[string]any {
		return map[string]any{"contentFilteringDefault": filtering, "knownLimitations": []any{}, "provenanceMarking": marking}
	}
	for name, options := range map[string]harnessOptions{
		"no catalogue":                    {imageSafety: allowingImageSafety()},
		"an undeclared line":              {imageSafety: allowingImageSafety(), catalogue: mustCatalogue(t, []byte(declaredCatalogue))},
		"no safety member":                {imageSafety: allowingImageSafety(), catalogue: imageCatalogue(t, nil)},
		"the provider's default filter":   {imageSafety: allowingImageSafety(), catalogue: imageCatalogue(t, declared("provider_default", "c2pa"))},
		"no filter":                       {imageSafety: allowingImageSafety(), catalogue: imageCatalogue(t, declared("none", "c2pa"))},
		"no provenance marking":           {imageSafety: allowingImageSafety(), catalogue: imageCatalogue(t, declared("strict", "none"))},
		"today's declaration":             {imageSafety: allowingImageSafety(), catalogue: imageCatalogue(t, declared("provider_default", "none"))},
		"a safe line but no check at all": {catalogue: imageCatalogue(t, safeImageLine)},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, options)
			failure := publicError(t, h.post("/v1/images/generations", validKey, imageBody(nil)), http.StatusBadRequest)
			if failure.Code != "unsupported_modality" || value(failure.Param) != "model" {
				t.Errorf("failure = %+v", failure)
			}
			if len(h.oxy.authorizationBodies()) != 0 || len(h.image.called()) != 0 {
				t.Errorf("%d authorizations, %d adapter calls; an unserved line holds nothing", len(h.oxy.authorizationBodies()), len(h.image.called()))
			}
			if safety, ok := options.imageSafety.(*scriptedImageSafety); ok {
				if prompts, _ := safety.asked(); prompts != 0 {
					t.Error("the check was asked about a line that is not served")
				}
			}
		})
	}
	// The control: every marking other than none, with strict filtering and
	// the check, is served on the same harness.
	for _, marking := range []string{"visible_watermark", "invisible_watermark", "c2pa"} {
		h := newHarness(t, harnessOptions{imageSafety: allowingImageSafety(), catalogue: imageCatalogue(t, declared("strict", marking))})
		if response := h.post("/v1/images/generations", validKey, imageBody(nil)); response.StatusCode != http.StatusOK {
			t.Errorf("strict with %s was refused: %d", marking, response.StatusCode)
		}
	}
}

// A safety member outside the contract's vocabulary stops the catalogue from
// loading: a misspelt marking would otherwise read as "not none".
func TestTheDeclaredSafetyMemberIsHeldToTheContractVocabulary(t *testing.T) {
	for name, safety := range map[string]map[string]any{
		"an unknown filtering word": {"contentFilteringDefault": "strictest", "knownLimitations": []any{}, "provenanceMarking": "c2pa"},
		"an unknown marking":        {"contentFilteringDefault": "strict", "knownLimitations": []any{}, "provenanceMarking": "watermark"},
		"no marking":                {"contentFilteringDefault": "strict", "knownLimitations": []any{}},
		"no filtering":              {"provenanceMarking": "c2pa"},
		"an unknown member":         {"contentFilteringDefault": "strict", "provenanceMarking": "c2pa", "classifier": "none"},
	} {
		if _, err := publicapi.ParsePublicCatalogue(imageCatalogueFile(t, safety)); err == nil || !strings.Contains(err.Error(), "safety") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := publicapi.ParsePublicCatalogue(imageCatalogueFile(t, safeImageLine)); err != nil {
		t.Fatalf("the well-formed member was refused: %v", err)
	}
}

func mustCatalogue(t *testing.T, raw []byte) *publicapi.PublicCatalogue {
	t.Helper()
	catalogue, err := publicapi.ParsePublicCatalogue(raw)
	if err != nil {
		t.Fatal(err)
	}
	return catalogue
}

func value(pointer *string) string {
	if pointer == nil {
		return ""
	}
	return *pointer
}

/* -------------------------------------------------------------------------- */
/*  The checked-in files                                                      */
/* -------------------------------------------------------------------------- */

const inventoryFilePath = "../../configs/inventory.json"

// declaredFormats answers ExecutesFormat from the reviewed table every adapter
// is pinned to (providerconfig.ExecutableAPIFormats), so the gate below reads
// the same statement serving does without building an adapter.
type declaredFormats struct{}

func (declaredFormats) ExecutesFormat(slug contract.ProviderSlug, format contract.APIFormat) (executes, known bool) {
	endpoint, known := providerconfig.Known[slug]
	if !known {
		return false, false
	}
	return slices.Contains(providerconfig.ExecutableAPIFormats(slug, endpoint.Protocol), format), true
}

type inventoryDeployment struct {
	DeploymentID    contract.DeploymentID   `json:"deploymentId"`
	Provider        contract.ProviderSlug   `json:"provider"`
	ModelReference  contract.ModelReference `json:"modelReference"`
	UpstreamModelID string                  `json:"upstreamModelId"`
}

// imageDeployments is the gate: of an inventory's deployments, the ones that
// could generate an image (the predicate serving uses), and of those the ones
// whose line the catalogue does not declare safe for it.
func imageDeployments(catalogue *publicapi.PublicCatalogue, deployments []inventoryDeployment) (generating, undeclared []contract.DeploymentID) {
	for _, deployment := range deployments {
		route := provider.Route{DeploymentID: deployment.DeploymentID, Provider: deployment.Provider, ModelReference: deployment.ModelReference, UpstreamModelID: deployment.UpstreamModelID}
		if !publicapi.ExecutesFamily(declaredFormats{}, route, contract.APIFormatImagesGenerations) {
			continue
		}
		generating = append(generating, deployment.DeploymentID)
		if !catalogue.DeclaresImageGenerationSafety(deployment.ModelReference.ModelID()) {
			undeclared = append(undeclared, deployment.DeploymentID)
		}
	}
	return generating, undeclared
}

// No deployment of configs/inventory.json may generate an image unless
// configs/public-catalogue.json declares its line with strict content
// filtering and a provenance marking (ADR 0013).
//
// The count of image-generating deployments is exact, and it is zero: Kaana's
// content check has no implementation yet, so nothing may serve the route.
// Raising it is the reviewed act of enabling image generation, and ADR 0013
// names what must be true first.
func TestNoCheckedInDeploymentGeneratesImagesWithoutDeclaredSafety(t *testing.T) {
	rawCatalogue, err := os.ReadFile(catalogueFilePath)
	if err != nil {
		t.Fatal(err)
	}
	catalogue := mustCatalogue(t, rawCatalogue)
	rawInventory, err := os.ReadFile(inventoryFilePath)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Deployments []inventoryDeployment `json:"deployments"`
	}
	if err := json.Unmarshal(rawInventory, &file); err != nil {
		t.Fatal(err)
	}
	// The vacuity floor: an emptied snapshot, or one whose shape this test no
	// longer reads, would satisfy the gate over nothing.
	if len(file.Deployments) < 100 {
		t.Fatalf("parsed %d deployments, want at least 100: the shape this test reads may have changed", len(file.Deployments))
	}
	for _, deployment := range file.Deployments {
		if deployment.DeploymentID == "" || deployment.Provider == "" || !deployment.ModelReference.Valid() || deployment.UpstreamModelID == "" {
			t.Fatalf("deployment %+v was not read whole", deployment)
		}
	}
	generating, undeclared := imageDeployments(catalogue, file.Deployments)
	if len(undeclared) != 0 {
		t.Errorf("%v could generate an image on a line the catalogue does not declare with strict content filtering and a provenance marking (ADR 0013)", undeclared)
	}
	if len(generating) != 0 {
		t.Errorf("%v generate images; the checked-in inventory serves none until Kaana's content check exists (ADR 0013, \"What enabling image generation needs\")", generating)
	}
}

// The positive control of the gate above: the same function, over an
// inventory that does carry image-generating deployments, names exactly the
// ones whose line is not declared safe.
func TestTheImageDeploymentGateNamesAnUndeclaredLine(t *testing.T) {
	line := func(model string, safety map[string]any) map[string]any {
		entry := map[string]any{
			"modelId": model, "publisher": map[string]any{"slug": "openai", "displayName": "OpenAI"}, "displayName": model,
			"capabilities": map[string]any{"inputModalities": []any{"text"}, "outputModalities": []any{"image"}, "tools": false, "parallelToolCalls": false,
				"structuredOutput": false, "jsonMode": false, "reasoning": false, "streaming": false, "promptCaching": false, "maxContextTokens": 32000, "maxOutputTokens": 1},
			"license":     map[string]any{"licenseId": "proprietary", "displayName": "Proprietary", "commercialUseAllowed": true, "requiresAttribution": false},
			"provenance":  map[string]any{"releaseKind": "third_party_hosted"},
			"dataPolicy":  map[string]any{"retainsPayloads": false, "retentionDays": 0, "trainsOnCustomerData": false, "zeroDataRetentionAvailable": true},
			"deprecation": map[string]any{"status": "active"},
		}
		if safety != nil {
			entry["safety"] = safety
		}
		return entry
	}
	declared := func(filtering, marking string) map[string]any {
		return map[string]any{"contentFilteringDefault": filtering, "knownLimitations": []any{}, "provenanceMarking": marking}
	}
	raw, err := json.Marshal(map[string]any{"schemaVersion": 1, "models": []any{
		line("openai/gpt-image-safe", declared("strict", "c2pa")),
		line("openai/gpt-image-default", declared("provider_default", "c2pa")),
		line("openai/gpt-image-unmarked", declared("strict", "none")),
		line("openai/gpt-image-silent", nil),
		line("openai/gpt-chat", declared("provider_default", "none")),
	}})
	if err != nil {
		t.Fatal(err)
	}
	catalogue := mustCatalogue(t, raw)
	deployment := func(id, slug, reference, upstream string) inventoryDeployment {
		return inventoryDeployment{DeploymentID: contract.DeploymentID(id), Provider: contract.ProviderSlug(slug), ModelReference: contract.ModelReference(reference), UpstreamModelID: upstream}
	}
	generating, undeclared := imageDeployments(catalogue, []inventoryDeployment{
		deployment("dep_safe", "openai", "openai/gpt-image-safe@v1", "gpt-image-1"),
		deployment("dep_default", "openai", "openai/gpt-image-default@v1", "gpt-image-1"),
		deployment("dep_unmarked", "openai", "openai/gpt-image-unmarked@v1", "dall-e-3"),
		deployment("dep_silent", "openai", "openai/gpt-image-silent@v1", "gpt-image-1"),
		deployment("dep_absent", "openai", "openai/gpt-image-absent@v1", "gpt-image-1"),
		// Not image deployments: a chat model on the slug that executes the
		// family, and an image model id on a slug whose adapter does not.
		deployment("dep_chat", "openai", "openai/gpt-chat@v1", "gpt-4.1"),
		deployment("dep_gateway", "openrouter", "openai/gpt-image-silent@v1", "openai/gpt-image-1"),
	})
	if want := []contract.DeploymentID{"dep_safe", "dep_default", "dep_unmarked", "dep_silent", "dep_absent"}; !slices.Equal(generating, want) {
		t.Errorf("generating = %v, want %v", generating, want)
	}
	if want := []contract.DeploymentID{"dep_default", "dep_unmarked", "dep_silent", "dep_absent"}; !slices.Equal(undeclared, want) {
		t.Errorf("undeclared = %v, want %v", undeclared, want)
	}
}
