package kaana

import (
	"reflect"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

func TestTokenEstimateIsIndependentOfStreamChunkBoundaries(t *testing.T) {
	var whole, chunked tokenEstimate
	whole.add("abcd世界efgh")
	for _, fragment := range []string{"ab", "cd世", "界e", "fgh"} {
		chunked.add(fragment)
	}
	if whole != chunked || whole.tokens() != chunked.tokens() {
		t.Fatalf("whole estimate = %+v/%d; chunked estimate = %+v/%d", whole, whole.tokens(), chunked, chunked.tokens())
	}
	if whole.tokens() != 4 {
		t.Fatalf("estimate = %d tokens, expected two ASCII groups plus two dense-script code points", whole.tokens())
	}
}

func TestUsageEstimateSeparatesVisibleAndReasoningOutput(t *testing.T) {
	text := "hello"
	estimate := newUsageEstimate(&contract.Request{Input: contract.Input{
		Format: contract.InputMessages,
		Messages: []contract.Message{{
			Role: contract.RoleUser, Content: []contract.ContentPart{{Type: contract.ContentPartText, Text: &text}},
		}},
	}})
	estimate.addDelta(contract.ChannelOutputText, "answer")
	estimate.addDelta(contract.ChannelReasoning, "think")
	estimate.addToolCall("lookup", `{"q":"kaana"}`)

	got := quantitiesByUnit(estimate.units())
	for _, unit := range []contract.UsageUnit{
		contract.UnitRequests, contract.UnitInputTokens, contract.UnitOutputTokens, contract.UnitReasoningTokens,
	} {
		if got[unit] <= 0 {
			t.Errorf("fallback has no positive %s quantity: %v", unit, got)
		}
	}
}

func TestUsageEstimateDoesNotInventUnitsFromInlineImageTransportBytes(t *testing.T) {
	request := func(data string) *contract.Request {
		return &contract.Request{Input: contract.Input{
			Format: contract.InputMessages,
			Messages: []contract.Message{{
				Role: contract.RoleUser,
				Content: []contract.ContentPart{{
					Type: contract.ContentPartImage,
					Source: &contract.ContentSource{
						Kind: contract.ContentSourceInline, MediaType: stringPointer("image/png"), Data: &data,
					},
				}},
			}},
		}}
	}

	small := newUsageEstimate(request("YQ==")).units()
	large := newUsageEstimate(request("YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh")).units()
	if !reflect.DeepEqual(small, large) {
		t.Fatalf("inline transport size changed token estimate: small=%v large=%v", small, large)
	}
	if _, invented := quantitiesByUnit(small)[contract.UnitImages]; invented {
		t.Fatalf("input image was reported under the ambiguous images unit: %v", small)
	}
}

func TestUsageEstimateDoesNotTreatInlineAudioBase64AsTextOrDuration(t *testing.T) {
	request := func(data string) *contract.Request {
		return &contract.Request{Input: contract.Input{
			Format: contract.InputMessages,
			Messages: []contract.Message{{
				Role: contract.RoleUser,
				Content: []contract.ContentPart{{
					Type: contract.ContentPartAudio,
					Source: &contract.ContentSource{
						Kind: contract.ContentSourceInline, MediaType: stringPointer("audio/wav"), Data: &data,
					},
				}},
			}},
		}}
	}

	small := newUsageEstimate(request("UklGRg==")).units()
	large := newUsageEstimate(request("UklGRmFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh")).units()
	if !reflect.DeepEqual(small, large) {
		t.Fatalf("inline audio transport size changed token estimate: small=%v large=%v", small, large)
	}
	quantities := quantitiesByUnit(small)
	if _, invented := quantities[contract.UnitAudioInputMilliseconds]; invented {
		t.Fatalf("audio duration was invented from base64 transport bytes: %v", small)
	}
}

func quantitiesByUnit(units []contract.UsageQuantity) map[contract.UsageUnit]int {
	quantities := make(map[contract.UsageUnit]int, len(units))
	for _, quantity := range units {
		quantities[quantity.Unit] = quantity.Quantity
	}
	return quantities
}

func stringPointer(value string) *string { return &value }
