package kaana

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// usageEstimate is the deterministic fallback for a provider that returns no
// usage after completing a request or after delivering partial output. It keeps
// only counters: prompt and output text never survive here, and no estimate is
// used when the provider supplied its own units.
//
// Tokenizers differ between models, so this deliberately does not pretend to
// reproduce one. ASCII text is estimated at four characters per token and
// every non-ASCII code point at one token. The latter keeps CJK and other dense
// scripts from being divided by an English-centric ratio. The report labels
// the result "estimated", which lets settlement distinguish it from provider
// accounting and reconcile it later.
type usageEstimate struct {
	input           tokenEstimate
	output          tokenEstimate
	reasoning       tokenEstimate
	outputDelivered bool
}

type tokenEstimate struct {
	asciiCharacters    int
	nonASCIICharacters int
}

func (e *tokenEstimate) add(text string) {
	for len(text) > 0 {
		r, size := utf8.DecodeRuneInString(text)
		text = text[size:]
		if r <= 0x7f {
			e.asciiCharacters++
		} else {
			e.nonASCIICharacters++
		}
	}
}

func (e tokenEstimate) tokens() int {
	return (e.asciiCharacters+3)/4 + e.nonASCIICharacters
}

func newUsageEstimate(request *contract.Request) *usageEstimate {
	estimate := &usageEstimate{}
	if request == nil {
		return estimate
	}

	switch request.Input.Format {
	case contract.InputMessages:
		for _, message := range request.Input.Messages {
			estimate.input.add(string(message.Role))
			if message.Name != nil {
				estimate.input.add(*message.Name)
			}
			if message.ToolCallID != nil {
				estimate.input.add(*message.ToolCallID)
			}
			for _, part := range message.Content {
				switch part.Type {
				case contract.ContentPartText, contract.ContentPartRefusal:
					if part.Text != nil {
						estimate.input.add(*part.Text)
					}
				case contract.ContentPartImage, contract.ContentPartAudio:
					// Providers meter decoded media, not its URL or inline base64
					// transport. Without decoded dimensions or duration there is
					// no honest unit to reconstruct here.
				case contract.ContentPartFile:
					if part.Filename != nil {
						estimate.input.add(*part.Filename)
					}
				}
			}
			for _, call := range message.ToolCalls {
				estimate.input.add(call.Name)
				estimate.input.add(call.Arguments)
			}
		}
	case contract.InputText:
		if request.Input.Text != nil {
			estimate.input.add(*request.Input.Text)
		}
	case contract.InputTextBatch:
		for _, text := range request.Input.Texts {
			estimate.input.add(text)
		}
	}

	for _, tool := range request.Tools {
		estimate.input.add(tool.Name)
		if tool.Description != nil {
			estimate.input.add(*tool.Description)
		}
		estimate.input.addJSON(tool.Parameters)
	}
	if request.ToolChoice != nil {
		if request.ToolChoice.Mode != nil {
			estimate.input.add(string(*request.ToolChoice.Mode))
		}
		if request.ToolChoice.Function != nil {
			estimate.input.add(request.ToolChoice.Function.Name)
		}
	}
	if request.ResponseFormat != nil {
		estimate.input.add(string(request.ResponseFormat.Type))
		if request.ResponseFormat.Name != nil {
			estimate.input.add(*request.ResponseFormat.Name)
		}
		estimate.input.addJSON(request.ResponseFormat.Schema)
	}
	return estimate
}

func (e *tokenEstimate) addJSON(value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		// Translate will refuse an unencodable request before it is sent. The
		// estimator is deliberately non-authoritative and must not introduce a
		// second validation path with different errors.
		return
	}
	e.add(string(encoded))
}

func (e *usageEstimate) addDelta(channel contract.DeltaChannel, text string) {
	if text != "" {
		e.outputDelivered = true
	}
	if channel == contract.ChannelReasoning {
		e.reasoning.add(text)
		return
	}
	e.output.add(text)
}

func (e *usageEstimate) addToolCall(name, arguments string) {
	// The id-only opening or closing event of a tool call is still model output
	// accepted by the sink, even when this particular frame has no tokenizable
	// name or argument fragment.
	e.outputDelivered = true
	e.output.add(name)
	e.output.add(arguments)
}

func (e *usageEstimate) hasDeliveredOutput() bool { return e.outputDelivered }

func (e *usageEstimate) units() []contract.UsageQuantity {
	units := []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}}
	if tokens := e.input.tokens(); tokens > 0 {
		units = append(units, contract.UsageQuantity{Unit: contract.UnitInputTokens, Quantity: tokens})
	}
	if tokens := e.output.tokens(); tokens > 0 {
		units = append(units, contract.UsageQuantity{Unit: contract.UnitOutputTokens, Quantity: tokens})
	}
	if tokens := e.reasoning.tokens(); tokens > 0 {
		units = append(units, contract.UsageQuantity{Unit: contract.UnitReasoningTokens, Quantity: tokens})
	}
	return units
}

// supplement keeps every provider-derived count and fills a missing or
// provisional estimate from output that the sink actually accepted. Some
// protocols report exact input usage at stream start but only report final
// output usage at stream end. A partial stream therefore has a non-empty unit
// list while its delivered output is still absent or lower than the
// deterministic reconstruction. Treating "non-empty" as "complete" loses the
// very partial usage settlement needs.
func (e *usageEstimate) supplement(units []contract.UsageQuantity) []contract.UsageQuantity {
	result := append([]contract.UsageQuantity(nil), units...)
	positions := make(map[contract.UsageUnit]int, len(result))
	for index, quantity := range result {
		positions[quantity.Unit] = index
	}
	for _, estimated := range e.units() {
		index, present := positions[estimated.Unit]
		if !present {
			positions[estimated.Unit] = len(result)
			result = append(result, estimated)
			continue
		}
		// Input and media counts reported at stream start remain authoritative.
		// Output and reasoning are the fields whose final totals disappear on a
		// partial stream; retain the strongest evidence available for each.
		if (estimated.Unit == contract.UnitOutputTokens || estimated.Unit == contract.UnitReasoningTokens) &&
			estimated.Quantity > result[index].Quantity {
			result[index].Quantity = estimated.Quantity
		}
	}
	return result
}
