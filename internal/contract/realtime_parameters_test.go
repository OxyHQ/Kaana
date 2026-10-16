package contract

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// parameterVectors is testdata/realtime-parameter-issues.json, which the
// TypeScript contract's realtimeParameterIssues is held to as well.
type parameterVectors struct {
	Statements map[string]RealtimeParameters `json:"statements"`
	Vectors    []struct {
		Name       string          `json:"name"`
		Parameters string          `json:"parameters"`
		Prefix     string          `json:"prefix"`
		Fields     json.RawMessage `json:"fields"`
		Params     []string        `json:"params"`
	} `json:"vectors"`
}

func readParameterVectors(t *testing.T) parameterVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/realtime-parameter-issues.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors parameterVectors
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	return vectors
}

// TestRealtimeParameterIssuesMatchTheSharedVectors is Kaana's own check
// against the vectors the published TypeScript check is held to, so a client
// that builds its session from a model's parameters is refused for nothing.
func TestRealtimeParameterIssuesMatchTheSharedVectors(t *testing.T) {
	vectors := readParameterVectors(t)
	for name, parameters := range vectors.Statements {
		if err := parameters.Validate(); err != nil {
			t.Errorf("statement %s: %v", name, err)
		}
	}
	for _, vector := range vectors.Vectors {
		parameters, ok := vectors.Statements[vector.Parameters]
		if !ok {
			t.Fatalf("%s names the unknown statement %q", vector.Name, vector.Parameters)
		}
		var fields RealtimeConfigFields
		switch vector.Prefix {
		case "config":
			var update RealtimeSessionConfigUpdate
			if err := json.Unmarshal(vector.Fields, &update); err != nil {
				t.Fatalf("%s: %v", vector.Name, err)
			}
			fields = update.Fields()
		case "response":
			var response RealtimeResponseParameters
			if err := json.Unmarshal(vector.Fields, &response); err != nil {
				t.Fatalf("%s: %v", vector.Name, err)
			}
			fields = response.Fields()
		default:
			t.Fatalf("%s has the prefix %q", vector.Name, vector.Prefix)
		}
		got := []string{}
		for _, issue := range parameters.Issues(vector.Prefix, fields) {
			got = append(got, issue.Param)
		}
		if !reflect.DeepEqual(got, vector.Params) {
			t.Errorf("%s: issues name %v, want %v", vector.Name, got, vector.Params)
		}
	}
}

func TestRealtimeParametersValidateRefusesWhatTheCatalogueCannotPublish(t *testing.T) {
	served := func() RealtimeParameters {
		return readParameterVectors(t).Statements["xai"]
	}
	for name, mutate := range map[string]func(*RealtimeParameters){
		"no auto":      func(p *RealtimeParameters) { p.ToolChoice = []RealtimeToolChoice{RealtimeToolChoiceNone} },
		"no detection": func(p *RealtimeParameters) { p.TurnDetection = nil },
		"an unsorted set": func(p *RealtimeParameters) {
			p.OutputModalities = [][]RealtimeOutputModality{{RealtimeOutputText, RealtimeOutputAudio}}
		},
		"a detection twice":    func(p *RealtimeParameters) { p.TurnDetection = append(p.TurnDetection, p.TurnDetection[0]) },
		"a range out of order": func(p *RealtimeParameters) { p.TurnDetection[1].Threshold = &NumberRange{Min: 0.9, Max: 0.1} },
		"no metered unit":      func(p *RealtimeParameters) { p.TurnDetection[0].MeteredUnits = nil },
	} {
		parameters := served()
		mutate(&parameters)
		if parameters.Validate() == nil {
			t.Errorf("%s validated", name)
		}
	}
}
