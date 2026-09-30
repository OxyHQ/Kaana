package inventory_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/provider"
)

func TestParseRefusesAnAcceptedParameterSetItCouldNotRepresent(t *testing.T) {
	now := time.Now()
	for name, observed := range map[string]string{
		"a provider's own word":   `{"acceptedParameters":["temperature"]}`,
		"a control Kaana refuses": `{"acceptedParameters":["sampling.topK"]}`,
		"an unsorted set":         `{"acceptedParameters":["tools","sampling.temperature"]}`,
		"a repeated control":      `{"acceptedParameters":["tools","tools"]}`,
	} {
		if _, err := inventory.Parse(issued(now, deploymentWith("d", "openrouter", observed)), time.Hour); err == nil || !strings.Contains(err.Error(), "acceptedParameters") {
			t.Errorf("%s: accepted or refused for another reason: %v", name, err)
		}
	}
	// Controls: the whole vocabulary, and an explicitly empty set, are accepted.
	whole, _ := json.Marshal(map[string]any{"acceptedParameters": provider.RequestParameters()})
	for _, observed := range []string{string(whole), `{"acceptedParameters":[]}`} {
		if _, err := inventory.Parse(issued(now, deploymentWith("d", "openrouter", observed)), time.Hour); err != nil {
			t.Errorf("control %s refused: %v", observed, err)
		}
	}
}

func TestTheVocabularyIsSortedAndClosed(t *testing.T) {
	vocabulary := provider.RequestParameters()
	if err := provider.ValidateParameterSet(vocabulary); err != nil {
		t.Fatalf("the vocabulary is not its own canonical order: %v", err)
	}
	if provider.RequestParameter("sampling.topK").Valid() {
		t.Error("sampling.topK entered the vocabulary; no discovered provider's adapter sends it")
	}
}

// TestARouteCarriesItsAcceptedParametersAndUnknownStaysUnknown is the one
// place observed metadata reaches a route. Nil must stay nil — an unknown set
// that became empty would refuse every control on every route.
func TestARouteCarriesItsAcceptedParametersAndUnknownStaysUnknown(t *testing.T) {
	document := issued(time.Now(), strings.Join([]string{
		deploymentWith("dep_known", "openrouter", `{"acceptedParameters":["maxOutputTokens","tools"]}`),
		deploymentWith("dep_empty", "cheaperinference", `{"acceptedParameters":[]}`),
		deploymentWith("dep_silent", "groq", `{"displayName":"x"}`),
	}, ","))
	loaded := parse(t, document)
	routes := map[contract.DeploymentID]provider.Route{}
	for _, id := range []contract.DeploymentID{"dep_known", "dep_empty", "dep_silent"} {
		route, err := loaded.Deployment(id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		routes[id] = route
	}

	known := routes["dep_known"]
	if known.AcceptedParameters == nil || !slices.Equal(*known.AcceptedParameters, []provider.RequestParameter{"maxOutputTokens", "tools"}) {
		t.Fatalf("known set = %v", known.AcceptedParameters)
	}
	if parameter, refused := known.UnacceptedParameter(provider.ParameterTools, provider.ParameterTemperature); !refused || parameter != provider.ParameterTemperature {
		t.Errorf("a known set must refuse what it lacks: %q %v", parameter, refused)
	}
	if _, refused := known.UnacceptedParameter(provider.ParameterTools, provider.ParameterMaxOutputTokens); refused {
		t.Error("control: a known set refused what it names")
	}
	if routes["dep_empty"].AcceptedParameters == nil {
		t.Error("an explicitly empty set became unknown")
	}
	if _, refused := routes["dep_empty"].UnacceptedParameter(provider.ParameterTools); !refused {
		t.Error("an explicitly empty set accepted a control")
	}
	if routes["dep_silent"].AcceptedParameters != nil {
		t.Errorf("a deployment that said nothing produced a set: %v", *routes["dep_silent"].AcceptedParameters)
	}
	if _, refused := routes["dep_silent"].UnacceptedParameter(provider.RequestParameters()...); refused {
		t.Error("an unknown set refused a control")
	}

	// A route owns its copy: mutating it cannot change the inventory.
	(*known.AcceptedParameters)[0] = "tools"
	again, _ := loaded.Deployment("dep_known")
	if (*again.AcceptedParameters)[0] != provider.ParameterMaxOutputTokens {
		t.Error("a route aliases the inventory's accepted-parameter slice")
	}
}

func TestCatalogueIntersectsAcceptedParametersAcrossReportersOnly(t *testing.T) {
	document := issued(time.Now(), strings.Join([]string{
		deploymentWith("dep_a", "openrouter", `{"acceptedParameters":["maxOutputTokens","sampling.temperature","tools"]}`),
		deploymentWith("dep_b", "cheaperinference", `{"acceptedParameters":["maxOutputTokens","tools"]}`),
		deploymentWith("dep_c", "groq", `{"displayName":"silent on parameters"}`),
	}, ","))
	entry := parse(t, document).Catalogue()[0]
	if entry.AcceptedParameters == nil || !slices.Equal(*entry.AcceptedParameters, []provider.RequestParameter{"maxOutputTokens", "tools"}) {
		t.Errorf("accepted parameters = %v, want the reporting intersection", entry.AcceptedParameters)
	}
	encoded, _ := json.Marshal(entry)
	if !strings.Contains(string(encoded), `"acceptedParameters":["maxOutputTokens","tools"]`) {
		t.Errorf("wire shape: %s", encoded)
	}

	// Control: nobody reporting leaves the field absent, not empty.
	silent := parse(t, issued(time.Now(), deploymentWith("dep_c", "groq", `{"displayName":"x"}`))).Catalogue()[0]
	if silent.AcceptedParameters != nil {
		t.Errorf("an unreported set became %v", *silent.AcceptedParameters)
	}
	if encoded, _ := json.Marshal(silent); strings.Contains(string(encoded), "acceptedParameters") {
		t.Errorf("an unreported set reached the wire: %s", encoded)
	}
}
