package contract

import "testing"

// TestATranscribersStatementNamesNoResponse: a statement that never responds
// names no output modality set and no tool choice and is valid; one that names
// a tool choice but no output set is still refused, so the exception cannot
// excuse a conversation statement that lost its modalities.
func TestATranscribersStatementNamesNoResponse(t *testing.T) {
	transcriber := RealtimeParameters{
		TurnDetection: []RealtimeTurnDetectionParameters{
			{Type: TurnDetectionNone, MeteredUnits: []UsageUnit{UnitSessionMilliseconds}},
			{Type: TurnDetectionServerVAD, MeteredUnits: []UsageUnit{UnitSessionMilliseconds}},
		},
		OutputModalities: [][]RealtimeOutputModality{},
		ToolChoice:       []RealtimeToolChoice{},
	}
	if err := transcriber.Validate(); err != nil {
		t.Fatalf("a transcriber's statement was refused: %v", err)
	}
	lost := transcriber
	lost.ToolChoice = []RealtimeToolChoice{RealtimeToolChoiceAuto}
	if err := lost.Validate(); err == nil {
		t.Fatal("a statement with a tool choice and no output modalities was accepted")
	}
	noDetection := transcriber
	noDetection.TurnDetection = nil
	if err := noDetection.Validate(); err == nil {
		t.Fatal("a transcriber with no turn detection was accepted")
	}
}
