package contract

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestDriftIsDetected is the positive control for the whole contract gate.
//
// TestGoTypesMatchPublishedShapes passing tells us nothing on its own: a
// comparator that resolved every field to "no opinion" would report exactly the
// same green. So this test perturbs the descriptor in each way the contract can
// actually drift, runs the SAME comparator over the SAME Go types, and requires
// each perturbation to be reported.
//
// The perturbations are the real failure modes, not arbitrary corruption: a
// field renamed, removed or added upstream; a required field made optional or
// the reverse; a scalar's type changed; a reference repointed; a versioned
// literal changed; a variant added to a discriminated union. If any of them
// comes back clean, the corresponding check in the gate is measuring nothing.
func TestDriftIsDetected(t *testing.T) {
	baseline := loadDescriptor(t)
	checker := &shapeChecker{descriptor: baseline}

	// Control on the control: the unperturbed comparison must be clean, or a
	// perturbation "being detected" would just be the pre-existing noise.
	if problems := checker.compareShape("inferenceRequestSchema", baseline.Shapes["inferenceRequestSchema"], reflect.TypeOf(Request{})); len(problems) != 0 {
		t.Fatalf("the unperturbed comparison is not clean, so every detection below is meaningless:\n%s", strings.Join(problems, "\n"))
	}

	cases := []struct {
		name    string
		shape   string
		goType  reflect.Type
		perturb func(*descriptorNode)
		expect  string
	}{
		{
			name:   "a field is renamed upstream",
			shape:  "inferenceRequestSchema",
			goType: reflect.TypeOf(Request{}),
			perturb: func(node *descriptorNode) {
				renameField(node, "stream", "streaming")
			},
			expect: `has JSON field "stream"`,
		},
		{
			name:   "a field is removed upstream",
			shape:  "inferenceRequestSchema",
			goType: reflect.TypeOf(Request{}),
			perturb: func(node *descriptorNode) {
				dropField(node, "modality")
			},
			expect: `has JSON field "modality"`,
		},
		{
			name:   "a required field is added upstream",
			shape:  "inferenceRequestSchema",
			goType: reflect.TypeOf(Request{}),
			perturb: func(node *descriptorNode) {
				node.Fields = append(node.Fields, descriptorNode{Name: "deadline", Kind: "string"})
			},
			expect: `the contract has field "deadline"`,
		},
		{
			name:   "a required field becomes optional upstream",
			shape:  "inferenceRequestSchema",
			goType: reflect.TypeOf(Request{}),
			perturb: func(node *descriptorNode) {
				editField(node, "stream", func(field *descriptorNode) { field.Optional = true })
			},
			expect: "the contract makes it optional",
		},
		{
			name:   "an optional field becomes required upstream",
			shape:  "inferenceRequestSchema",
			goType: reflect.TypeOf(Request{}),
			perturb: func(node *descriptorNode) {
				editField(node, "maxOutputTokens", func(field *descriptorNode) { field.Optional = false })
			},
			expect: "the contract requires it",
		},
		{
			name:   "a scalar changes type upstream",
			shape:  "inferenceRequestSchema",
			goType: reflect.TypeOf(Request{}),
			perturb: func(node *descriptorNode) {
				editField(node, "stream", func(field *descriptorNode) { field.Kind = "string" })
			},
			expect: "the contract says string",
		},
		{
			name:   "a reference is repointed upstream",
			shape:  "inferenceRequestSchema",
			goType: reflect.TypeOf(Request{}),
			perturb: func(node *descriptorNode) {
				editField(node, "modality", func(field *descriptorNode) { field.Ref = "usageUnitSchema" })
			},
			expect: "references enum usageUnitSchema",
		},
		{
			name:   "a version literal changes type upstream",
			shape:  "inferenceStreamStartEventSchema",
			goType: reflect.TypeOf(StreamStartEvent{}),
			perturb: func(node *descriptorNode) {
				editField(node, "schemaVersion", func(field *descriptorNode) {
					field.Value = json.RawMessage(`"one"`)
				})
			},
			expect: "pins the literal",
		},
		{
			name:   "an integer becomes a float upstream",
			shape:  "inferenceStreamDeltaEventSchema",
			goType: reflect.TypeOf(StreamDeltaEvent{}),
			perturb: func(node *descriptorNode) {
				editField(node, "outputIndex", func(field *descriptorNode) {
					field.Constraints = map[string]any{}
				})
			},
			expect: "the contract says number",
		},
		{
			name:   "a variant is added to a discriminated union upstream",
			shape:  "routingTargetSchema",
			goType: reflect.TypeOf(RoutingTarget{}),
			perturb: func(node *descriptorNode) {
				node.Variants = append(node.Variants, descriptorNode{
					Kind:   "object",
					Strict: true,
					Fields: []descriptorNode{
						{Name: "kind", Kind: "literal", Value: json.RawMessage(`"deployment"`)},
						{Name: "deploymentId", Kind: "ref", Ref: "deploymentIdSchema"},
					},
				})
			},
			expect: "enum members differ",
		},
		{
			name:   "an array's element type changes upstream",
			shape:  "normalizedUsageReportSchema",
			goType: reflect.TypeOf(UsageReport{}),
			perturb: func(node *descriptorNode) {
				editField(node, "units", func(field *descriptorNode) {
					field.Items = &descriptorNode{Kind: "string"}
				})
			},
			expect: "the contract says string",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			perturbed := cloneNode(t, baseline.Shapes[testCase.shape])
			testCase.perturb(&perturbed)

			// The perturbation must have applied. A no-op edit and a surviving
			// mutation are indistinguishable from the outcome alone.
			if reflect.DeepEqual(perturbed, baseline.Shapes[testCase.shape]) {
				t.Fatalf("the perturbation changed nothing, so its detection would be meaningless")
			}

			problems := checker.compareShape(testCase.shape, perturbed, testCase.goType)
			if len(problems) == 0 {
				t.Fatalf("drift went undetected: %s", testCase.name)
			}
			if !strings.Contains(strings.Join(problems, "\n"), testCase.expect) {
				t.Errorf("drift was reported, but not as the expected failure %q:\n%s", testCase.expect, strings.Join(problems, "\n"))
			}
		})
	}
}

// TestVocabularyDiffDetectsBothDirections is the positive control for
// diffStringLists, which every enum and constant check is built on. An
// always-empty implementation would silently green all of them.
func TestVocabularyDiffDetectsBothDirections(t *testing.T) {
	if diff := diffStringLists([]string{"a", "b"}, []string{"a", "b"}); diff != "" {
		t.Errorf("identical vocabularies reported a difference: %s", diff)
	}
	if diff := diffStringLists([]string{"a", "b"}, []string{"a"}); !strings.Contains(diff, "absent from Go: [b]") {
		t.Errorf("a member missing from Go went unreported: %q", diff)
	}
	if diff := diffStringLists([]string{"a"}, []string{"a", "b"}); !strings.Contains(diff, "absent from the contract: [b]") {
		t.Errorf("a member Go invented went unreported: %q", diff)
	}
	// Order is not meaning: the contract's declaration order is a source-file
	// detail and reordering it must not fail the build.
	if diff := diffStringLists([]string{"a", "b"}, []string{"b", "a"}); diff != "" {
		t.Errorf("reordering reported a difference: %s", diff)
	}
}

func cloneNode(t *testing.T, node descriptorNode) descriptorNode {
	t.Helper()
	encoded, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("cloning a descriptor node: %v", err)
	}
	var clone descriptorNode
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatalf("cloning a descriptor node: %v", err)
	}
	return clone
}

func editField(node *descriptorNode, name string, edit func(*descriptorNode)) {
	for index := range node.Fields {
		if node.Fields[index].Name == name {
			edit(&node.Fields[index])
			return
		}
	}
	panic("no field named " + name + " to perturb")
}

func renameField(node *descriptorNode, from, to string) {
	editField(node, from, func(field *descriptorNode) { field.Name = to })
}

func dropField(node *descriptorNode, name string) {
	kept := node.Fields[:0]
	found := false
	for _, field := range node.Fields {
		if field.Name == name {
			found = true
			continue
		}
		kept = append(kept, field)
	}
	if !found {
		panic("no field named " + name + " to drop")
	}
	node.Fields = kept
}
