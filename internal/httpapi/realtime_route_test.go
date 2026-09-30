package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestTheRealtimeRouteIsMountedAndSigned: GET /internal/v1/realtime reaches
// the session manager, which refuses an unsigned first frame with a policy
// close; any other method is refused by the router before an upgrade.
func TestTheRealtimeRouteIsMountedAndSigned(t *testing.T) {
	h := newHarness(t, &stubAdapter{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.server.URL, "http")+"/internal/v1/realtime", nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatalf("the realtime route did not upgrade: %v", err)
	}
	defer func() { _ = ws.CloseNow() }()
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"schemaVersion":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("an unsigned first frame ended with %v, want a policy close", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/internal/v1/realtime", nil)
	if err != nil {
		t.Fatal(err)
	}
	posted, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = posted.Body.Close()
	if posted.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /internal/v1/realtime = %d", posted.StatusCode)
	}
}
