package publicapi

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
	"github.com/OxyHQ/Kaana/internal/publisher"
)

// servingKinds is a registry as one Kaana task sees it: `xai` executes
// requests, `xai-stt` holds transcription sessions, `xai-realtime`
// conversations; anything else this task does not serve.
type servingKinds map[contract.ProviderSlug][]contract.RealtimeSessionKind

func (k servingKinds) ServingKinds(slug contract.ProviderSlug) ([]contract.RealtimeSessionKind, bool, bool) {
	kinds, known := k[slug]
	if !known {
		return nil, false, false
	}
	return kinds, kinds == nil, true
}

var oneTask = servingKinds{
	"xai":          nil,
	"xai-stt":      {contract.RealtimeTranscription},
	"xai-realtime": {contract.RealtimeConversation},
}

// twoWayLine is one model line served both ways: file transcription under
// `xai`, a streaming session under `xai-stt`, and a third route on a slug this
// task does not serve.
func twoWayLine(t *testing.T) *inventory.Inventory {
	t.Helper()
	deployment := func(id, slug string) map[string]any {
		return map[string]any{
			"deploymentId": id, "provider": slug, "current": true,
			"modelReference": "x-ai/grok-voice-transcribe-2.0@observed-2026-10-16", "upstreamModelId": "grok-voice-transcribe-2.0",
		}
	}
	document, err := json.Marshal(map[string]any{
		"snapshotId": "snap_two_way", "issuedAt": contract.NewTimestamp(time.Now()),
		"deployments": []map[string]any{
			deployment("dep_a_xai_file", "xai"),
			deployment("dep_b_xai_stream", "xai-stt"),
			deployment("dep_c_elsewhere", "elsewhere"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := inventory.Parse(document, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return current
}

func twoWayGrant(operation contract.GrantOperation) *contract.Grant {
	line := contract.ModelID("x-ai/grok-voice-transcribe-2.0")
	return &contract.Grant{
		RequestID: "req_two_way", Operation: operation,
		Steps: []contract.GrantStep{{
			Target: contract.GrantTarget{Kind: contract.GrantTargetModel, ModelLine: &line},
			Limits: contract.GrantStepLimits{AdmittedModelLines: []contract.AdmittedModelLine{{ModelLine: line, PriceVersionID: "price"}}},
		}},
	}
}

func routedDeployments(t *testing.T, server *Server, operation contract.GrantOperation, kind *contract.RealtimeSessionKind) []contract.DeploymentID {
	t.Helper()
	resolved, failure := resolveGrant(twoWayGrant(operation), twoWayLine(t), nil, server.servesOperation(operation, kind), time.Now())
	if failure != nil {
		return nil
	}
	var deployments []contract.DeploymentID
	for _, route := range resolved.routes {
		deployments = append(deployments, route.DeploymentID)
	}
	return deployments
}

// TestAGrantIsRoutedOnlyToAdaptersThatPerformItsOperation: Oxy admits a line
// and allows every provider that serves it, so Kaana keeps the routes whose
// adapter can perform the operation, and leaves a slug it does not serve to
// the executor exactly as before.
func TestAGrantIsRoutedOnlyToAdaptersThatPerformItsOperation(t *testing.T) {
	server := &Server{kinds: oneTask}
	transcription := contract.RealtimeTranscription
	conversation := contract.RealtimeConversation

	if got := routedDeployments(t, server, contract.GrantAudioTranscriptions, nil); !slices.Equal(got, []contract.DeploymentID{"dep_a_xai_file", "dep_c_elsewhere"}) {
		t.Errorf("a file transcription was routed to %v", got)
	}
	if got := routedDeployments(t, server, contract.GrantRealtimeSession, &transcription); !slices.Equal(got, []contract.DeploymentID{"dep_b_xai_stream", "dep_c_elsewhere"}) {
		t.Errorf("a transcription session was routed to %v", got)
	}
	if got := routedDeployments(t, server, contract.GrantRealtimeSession, &conversation); !slices.Equal(got, []contract.DeploymentID{"dep_c_elsewhere"}) {
		t.Errorf("a conversation was routed to %v, a slug that holds only transcription sessions included", got)
	}

	// Control: without the registry's knowledge every route of the line is
	// resolved, which is what made a session reach the request adapter.
	unfiltered := &Server{}
	if got := routedDeployments(t, unfiltered, contract.GrantRealtimeSession, &transcription); len(got) != 3 {
		t.Errorf("without kinds, %v", got)
	}
}

// TestALineServedBothWaysKeepsItsUploadsAndGainsTheSession: the catalogue
// entry of x-ai/grok-voice-transcribe-2.0, served as file transcription under
// `xai` and as a session under `xai-stt`, keeps the uploads and languages the
// file transcription states (the session states no audio, so it narrows
// nothing) and publishes the session's realtime statement (file transcription
// states none, so it abstains).
func TestALineServedBothWaysKeepsItsUploadsAndGainsTheSession(t *testing.T) {
	deployment := func(id, slug, protocol string) map[string]any {
		entry := map[string]any{
			"deploymentId": id, "provider": slug, "current": true,
			"modelReference": "x-ai/grok-voice-transcribe-2.0@observed-2026-10-16", "upstreamModelId": "grok-voice-transcribe-2.0",
		}
		if observed := publisher.StatedObservation(contract.ProviderSlug(slug), protocol, "grok-voice-transcribe-2.0"); observed != nil {
			entry["observed"] = observed
		}
		return entry
	}
	build := func(deployments ...map[string]any) inventory.CatalogueEntry {
		t.Helper()
		document, err := json.Marshal(map[string]any{"snapshotId": "snap_both_ways", "issuedAt": contract.NewTimestamp(time.Now()), "deployments": deployments})
		if err != nil {
			t.Fatal(err)
		}
		current, err := inventory.Parse(document, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		entries := current.Catalogue()
		if len(entries) != 1 {
			t.Fatalf("catalogue = %+v", entries)
		}
		return entries[0]
	}
	file := deployment("dep_a_file", "xai", providerconfig.ProtocolOpenAICompatible)
	stream := deployment("dep_b_stream", "xai-stt", providerconfig.ProtocolXAISTT)

	alone := build(file)
	both := build(file, stream)
	if alone.Audio == nil || len(alone.Audio.InputMediaTypes) == 0 {
		t.Fatal("control: file transcription alone states no uploads, so this test proves nothing")
	}
	if both.Audio == nil || !slices.Equal(both.Audio.InputMediaTypes, alone.Audio.InputMediaTypes) || !slices.Equal(both.Audio.Languages, alone.Audio.Languages) {
		t.Errorf("serving the line as a session too changed its audio: %+v, was %+v", both.Audio, alone.Audio)
	}
	if both.Realtime == nil || !slices.Equal(both.Realtime.SessionKinds, []contract.RealtimeSessionKind{contract.RealtimeTranscription}) {
		t.Errorf("the line does not publish its transcription session: %+v", both.Realtime)
	}
	if alone.Realtime != nil {
		t.Errorf("file transcription alone published a session: %+v", alone.Realtime)
	}
}
