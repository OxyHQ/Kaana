package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// xAI voice discovery.
//
// xAI's authenticated GET /v1/models does not list its Voice Agent models, and
// the endpoints that would (GET /v1/realtime/models, /v1/realtime/voices)
// answer an ordinary team key 403. Measured with the production key on
// 2026-09-30. What xAI does answer is the session itself: on
// `wss://api.x.ai/v1/realtime?model=<id>` it sends `session.created`
// unprompted, and that event's `session.model` is the model xAI will run the
// session on (https://docs.x.ai/voice-realtime.ws.json, serverMessages).
//
// The handshake does NOT validate the model. Probed on 2026-09-30, a bogus
// `?model=` and no `?model=` at all were both upgraded (101) and both answered
// `session.created` with `"model":"grok-voice-think-fast-2.0"`: xAI substitutes
// its default silently. So the upgrade proves nothing, and neither does the
// existence of a session.created: only a session.created whose `session.model`
// is EXACTLY the id Kaana asked for is evidence that the account is served that
// model. A different model there is xAI saying it would serve something else
// under that name, which is precisely what a pinned reference must never
// resolve to.
//
// The probe is read-only: nothing is written to the socket, no audio exists,
// and the connection is closed on the first answer. xAI bills audio sent and
// received on a push-to-talk session, and `conversation.item.create` events;
// the session it opened reported `turn_detection: {"type": null}` (push-to-talk),
// and the probe sends neither (docs/inventory.md, "xAI voice discovery").
//
// Failure follows the speech profile's precedent (Discover, /tts/voices): an
// answer — an `error` event, or session.created naming another model — is a
// definitive "not served" and leaves the voice model absent; a probe that got
// no answer (a refused handshake, a close, a timeout, a frame it cannot read)
// fails that provider's discovery for the cycle, so the voice line is absent
// either way and the other providers are unaffected (PublishOnce).

// xaiRealtimeProbeTimeout bounds one probe, handshake to first answer. The
// production open answered session.created within ~150 ms of the upgrade.
var xaiRealtimeProbeTimeout = 10 * time.Second

// maxXAIRealtimeProbeEvents bounds the events read before session.created. The
// documented and observed order puts it first; conversation.created and xAI's
// JSON `ping` may follow, and anything else before it is not the documented
// open.
const maxXAIRealtimeProbeEvents = 4

// maxXAIRealtimeProbeEventBytes bounds one event. session.created is a few
// hundred bytes.
const maxXAIRealtimeProbeEventBytes = 64 << 10

func discoverXAIRealtimeSessions(ctx context.Context, client *http.Client, target Provider, listed map[string]struct{}) ([]DiscoveredModel, error) {
	if target.Slug != "xai-realtime" {
		return nil, fmt.Errorf("publisher: xAI voice discovery requires the xai-realtime provider")
	}
	models := make([]DiscoveredModel, 0, len(target.AttributedModels))
	for _, id := range target.AttributedModels {
		if _, already := listed[id]; already {
			// The account list named it: that is the existing evidence, and a
			// session would only ask the same question again.
			continue
		}
		family, _, _ := providerconfig.ClassifyModel(target.Slug, id)
		if family.Session != contract.RealtimeConversation {
			// Only a voice model is served by a session; anything else
			// attributed here is dropped by the snapshot builder anyway.
			continue
		}
		served, err := probeXAIRealtimeSession(ctx, client, target, id)
		if err != nil {
			return nil, err
		}
		if served {
			models = append(models, DiscoveredModel{UpstreamModelID: id})
		}
	}
	return models, nil
}

// probeXAIRealtimeSession opens one session naming model, reads xAI's answer
// and closes. It reports whether xAI said it runs the session on exactly that
// model; an error means xAI gave no answer at all.
func probeXAIRealtimeSession(ctx context.Context, client *http.Client, target Provider, model string) (bool, error) {
	endpoint, err := xaiRealtimeSessionEndpoint(target, model)
	if err != nil {
		return false, err
	}
	probeContext, cancel := context.WithTimeout(ctx, xaiRealtimeProbeTimeout)
	defer cancel()

	header := http.Header{}
	header.Set("Authorization", "Bearer "+target.APIKey)
	conn, response, err := websocket.Dial(probeContext, endpoint, &websocket.DialOptions{
		HTTPClient: provider.RefuseRedirects(client),
		HTTPHeader: header,
	})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if response != nil && response.StatusCode != http.StatusSwitchingProtocols {
			// The refusal body is xAI's and may quote the request; only the
			// status leaves this function.
			return false, fmt.Errorf("publisher: %s refused a voice session for %q with status %d", target.Slug, model, response.StatusCode)
		}
		return false, fmt.Errorf("publisher: opening a %s voice session for %q: %s", target.Slug, model, provider.RedactSecret(err.Error(), target.APIKey))
	}
	// Nothing is ever written: the close below is the only frame Kaana sends.
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	conn.SetReadLimit(maxXAIRealtimeProbeEventBytes)

	for range maxXAIRealtimeProbeEvents {
		kind, data, err := conn.Read(probeContext)
		if err != nil {
			if errors.Is(probeContext.Err(), context.DeadlineExceeded) {
				return false, fmt.Errorf("publisher: %s's voice session for %q sent no session.created within %s", target.Slug, model, xaiRealtimeProbeTimeout)
			}
			return false, fmt.Errorf("publisher: %s's voice session for %q ended before session.created: %s", target.Slug, model, provider.RedactSecret(err.Error(), target.APIKey))
		}
		if kind != websocket.MessageText {
			return false, fmt.Errorf("publisher: %s's voice session for %q sent a non-JSON frame before session.created", target.Slug, model)
		}
		var event struct {
			Type    string `json:"type"`
			Session *struct {
				Model string `json:"model"`
			} `json:"session"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			return false, fmt.Errorf("publisher: %s's voice session for %q sent an event that is not JSON", target.Slug, model)
		}
		switch event.Type {
		case "error":
			// xAI's own answer about this session on this key: not served.
			return false, nil
		case "session.created":
			if event.Session == nil {
				return false, fmt.Errorf("publisher: %s's session.created for %q carries no session", target.Slug, model)
			}
			return event.Session.Model == model, nil
		}
	}
	return false, fmt.Errorf("publisher: %s's voice session for %q sent %d events without session.created", target.Slug, model, maxXAIRealtimeProbeEvents)
}

// xaiRealtimeSessionEndpoint is the session URL on the provider's own root:
// `{base}/realtime?model=<id>`. For the locked root https://api.x.ai/v1 that
// is exactly providerconfig.XAIRealtimeSessionURL (the WebSocket client reads
// https as wss), so the key goes to the endpoint the serving adapter dials and
// nowhere else.
func xaiRealtimeSessionEndpoint(target Provider, model string) (string, error) {
	parsed, err := url.Parse(strings.TrimSuffix(target.BaseURL, "/") + "/realtime")
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("publisher: provider %s has an invalid voice session address", target.Slug)
	}
	query := url.Values{}
	query.Set("model", model)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
