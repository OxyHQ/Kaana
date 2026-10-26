package realtime_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/kaana"
	fake "github.com/OxyHQ/Kaana/internal/provider/openairealtime/openairealtimetest"
	"github.com/OxyHQ/Kaana/internal/realtime"
	"github.com/OxyHQ/Kaana/internal/sessionowner"
	"github.com/coder/websocket"
)

type memoryOwners struct {
	mu             sync.Mutex
	rows           map[string]sessionowner.Entry
	failedRenewals atomic.Int32
}

func (d *memoryOwners) Claim(_ context.Context, e sessionowner.Entry) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if old, ok := d.rows[e.RequestID]; ok && old.ExpiresAt.After(time.Now()) {
		return false, nil
	}
	e.ExpiresAt = time.Now().Add(sessionowner.LeaseDuration)
	d.rows[e.RequestID] = e
	return true, nil
}
func (d *memoryOwners) Lookup(_ context.Context, id, lane string) (sessionowner.Entry, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.rows[id]
	return e, ok && e.Lane == lane && e.ExpiresAt.After(time.Now()), nil
}
func (d *memoryOwners) Renew(_ context.Context, e sessionowner.Entry) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	old, ok := d.rows[e.RequestID]
	if !ok || old.Generation != e.Generation || old.Address != e.Address || old.Lane != e.Lane || !old.ExpiresAt.After(time.Now()) {
		d.failedRenewals.Add(1)
		return false, nil
	}
	old.ExpiresAt = time.Now().Add(sessionowner.LeaseDuration)
	d.rows[e.RequestID] = old
	return true, nil
}
func (d *memoryOwners) Release(_ context.Context, e sessionowner.Entry) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	old := d.rows[e.RequestID]
	if old.Generation == e.Generation && old.Address == e.Address && old.Lane == e.Lane {
		delete(d.rows, e.RequestID)
	}
	return nil
}

// Only the test dialer maps the fixed private :8080 address to httptest. The
// actual HTTP Host, private ingress, generation handshake and WS relay run.
func peerPair(t *testing.T, gate realtime.PublicGate) (*harness, *harness, *memoryOwners) {
	t.Helper()
	directory := &memoryOwners{rows: map[string]sessionowner.Entry{}}
	var destination string
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "127.0.0.1:8080" {
			return nil, errors.New("unexpected peer destination")
		}
		return (&net.Dialer{}).DialContext(ctx, network, destination)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	config := func(ip, generation string) *realtime.PeerConfig {
		return &realtime.PeerConfig{Directory: directory, Address: netip.MustParseAddr(ip), Subnet: netip.MustParsePrefix("127.0.0.0/8"), Generation: generation, HTTPClient: &http.Client{Transport: transport}}
	}
	owner := newHarness(t, options{gate: gate, peers: config("127.0.0.1", strings.Repeat("a", 32))})
	destination = owner.server.Listener.Addr().String()
	other := newHarness(t, options{gate: gate, private: owner.private, peers: config("127.0.0.2", strings.Repeat("b", 32))})
	return owner, other, directory
}

func TestPrivatePeerResumeReplaysWithoutOpeningOrSettlingTwice(t *testing.T) {
	owner, other, directory := peerPair(t, nil)
	owner.upstream.Script = func(conn *fake.Conn) {
		conn.Expect("input_audio_buffer.append")
		spokenTurn(conn, "response_peer", fake.Usage(2, 0, 0, 0, 3, 0))
	}
	const id = contract.RequestID("peer_internal")
	first := owner.open(sessionRequest(id, primary))
	read(t, first)
	command(t, first, appendAudio(id, "audio_once", "AAAA"))
	before := readUntil(t, first, "response.done")
	_ = first.CloseNow()
	second := other.connect(encodeCommand(t, resumeCommand(id, "resume", 0)), nil, nil)
	frames := readUntil(t, second, "session.resumed")
	if len(frames) != len(before)+1 {
		t.Fatalf("lost replay: %s", describe(frames))
	}
	requireContiguous(t, frames, 1)
	command(t, second, appendAudio(id, "audio_once", "AAAA"))
	duplicate := read(t, second)
	if duplicate["duplicate"] != true {
		t.Fatalf("command replay executed: %v", duplicate)
	}
	command(t, second, closeCommand(id, "close"))
	readUntil(t, second, "session.closed")
	read(t, second)
	expectClose(t, second, websocket.StatusNormalClosure)
	waitFor(t, "owner settlement", func() bool { return len(owner.costs.recorded()) == 1 && owner.manager.Open() == 0 })
	if owner.upstream.Dials() != 1 || other.upstream.Dials() != 0 || len(other.costs.recorded()) != 0 {
		t.Fatal("relay admitted a second execution")
	}
	if _, found, _ := directory.Lookup(context.Background(), string(id), sessionowner.Internal); found {
		t.Fatal("ended session retained directory lease")
	}
}

type countedPublicGate struct {
	admits  atomic.Int32
	settles atomic.Int32
}

func (g *countedPublicGate) Credential(r *http.Request) ([32]byte, bool) {
	value := r.Header.Get("Authorization")
	return sha256.Sum256([]byte(value)), value != ""
}
func (g *countedPublicGate) OriginPatterns() []string { return nil }
func (g *countedPublicGate) Admit(_ context.Context, r *http.Request, _ []byte) (*realtime.PublicAdmission, *contract.PublicError) {
	g.admits.Add(1)
	request := sessionRequest("peer_public", primary)
	digest, _ := g.Credential(r)
	return &realtime.PublicAdmission{Request: &request, Credential: digest, Settle: func(context.Context, kaana.SessionSettlement, *kaana.SessionOpening) { g.settles.Add(1) }}, nil
}
func publicConnect(t *testing.T, h *harness, token string, frame []byte) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.server.URL, "http")+"/v1/realtime", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{token}}, Subprotocols: []string{"kaana.v1"}})
	closeBody(response)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	if err = ws.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	return ws
}
func TestPrivatePeerPublicResumeKeepsOriginalCredentialAndAdmission(t *testing.T) {
	gate := &countedPublicGate{}
	owner, other, _ := peerPair(t, gate)
	first := publicConnect(t, owner, "Bearer original", []byte(`{"type":"session.open"}`))
	read(t, first)
	_ = first.CloseNow()
	resume := encodeCommand(t, resumeCommand("peer_public", "resume", 0))
	wrong := publicConnect(t, other, "Bearer different", resume)
	if got := read(t, wrong); got.kind() != "error" {
		t.Fatalf("different credential resumed: %v", got)
	}
	expectClose(t, wrong, websocket.StatusPolicyViolation)
	second := publicConnect(t, other, "Bearer original", resume)
	readUntil(t, second, "session.resumed")
	command(t, second, closeCommand("peer_public", "close"))
	readUntil(t, second, "session.closed")
	expectClose(t, second, websocket.StatusNormalClosure)
	waitFor(t, "public settlement", func() bool { return gate.settles.Load() == 1 })
	if gate.admits.Load() != 1 || owner.upstream.Dials() != 1 || other.upstream.Dials() != 0 {
		t.Fatal("resume consumed another admission")
	}
}

func TestPrivatePeerFailedOwnershipReleasesUnservedPublicAdmission(t *testing.T) {
	gate := &countedPublicGate{}
	owner, _, directory := peerPair(t, gate)
	directory.mu.Lock()
	directory.rows["peer_public"] = sessionowner.Entry{RequestID: "peer_public", Lane: sessionowner.Public, Address: "127.0.0.2", Generation: strings.Repeat("c", 32), ExpiresAt: time.Now().Add(time.Minute)}
	directory.mu.Unlock()
	ws := publicConnect(t, owner, "Bearer original", []byte(`{"type":"session.open"}`))
	if event := read(t, ws); event.kind() != "error" {
		t.Fatal("ownership failure announced a session", event)
	}
	if gate.admits.Load() != 1 || gate.settles.Load() != 1 || owner.upstream.Dials() != 0 || owner.manager.Open() != 0 {
		t.Fatal("ownership refusal lost admission or opened provider")
	}
	row, found, _ := directory.Lookup(context.Background(), "peer_public", sessionowner.Public)
	if !found || row.Generation != strings.Repeat("c", 32) {
		t.Fatal("refusal removed another owner")
	}
}

func TestPrivatePeerGenerationMismatchNeverForwardsCredentials(t *testing.T) {
	owner, other, directory := peerPair(t, nil)
	const id = contract.RequestID("peer_reused_ip")
	ws := owner.open(sessionRequest(id, primary))
	read(t, ws)
	_ = ws.CloseNow()
	directory.mu.Lock()
	row := directory.rows[string(id)]
	row.Generation = strings.Repeat("c", 32)
	directory.rows[string(id)] = row
	directory.mu.Unlock()
	resumed := other.connect(encodeCommand(t, resumeCommand(id, "resume", 0)), nil, nil)
	if got := read(t, resumed); got.kind() != "error" {
		t.Fatalf("accepted replaced process: %v", got)
	}
	expectClose(t, resumed, websocket.StatusPolicyViolation)
	if other.upstream.Dials() != 0 || owner.upstream.Dials() != 1 {
		t.Fatal("generation failure opened provider")
	}
	// Restore only for cleanup; no request can mutate the directory generation.
	directory.mu.Lock()
	row.Generation = strings.Repeat("a", 32)
	directory.rows[string(id)] = row
	directory.mu.Unlock()
}

func TestPrivatePeerIngressRefusesALBAndForeignHosts(t *testing.T) {
	owner, _, _ := peerPair(t, nil)
	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Proto", "CF-Connecting-IP"} {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080"+realtime.PeerPath, nil)
		r.RemoteAddr = "127.0.0.2:1000"
		r.Header.Set(header, "test")
		w := httptest.NewRecorder()
		owner.manager.PeerHandler().ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s admitted", header)
		}
	}
	for _, pair := range [][2]string{{"127.0.0.1:8080", "1.2.3.4:1234"}, {"kaana.ai", "127.0.0.2:1234"}, {"127.0.0.1:9000", "127.0.0.2:1234"}} {
		r := httptest.NewRequest(http.MethodGet, "http://"+pair[0]+realtime.PeerPath, nil)
		r.RemoteAddr = pair[1]
		w := httptest.NewRecorder()
		owner.manager.PeerHandler().ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatal("foreign ingress admitted")
		}
	}
}

func TestPrivatePeerReusedAddressReceivesNoCredentialsOrResumeFrame(t *testing.T) {
	var frames atomic.Int32
	var headers atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Oxy-Signature") != "" || r.Header.Get("Origin") != "" {
			headers.Add(1)
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err = ws.Write(ctx, websocket.MessageText, []byte(`{"generation":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`)); err != nil {
			return
		}
		if _, _, err = ws.Read(ctx); err == nil {
			frames.Add(1)
		}
	}))
	defer peer.Close()
	directory := &memoryOwners{rows: map[string]sessionowner.Entry{"peer_reuse": {RequestID: "peer_reuse", Lane: sessionowner.Public, Address: "127.0.0.1", Generation: strings.Repeat("a", 32), ExpiresAt: time.Now().Add(time.Minute)}}}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "127.0.0.1:8080" {
			return nil, errors.New("foreign peer")
		}
		return (&net.Dialer{}).DialContext(ctx, network, peer.Listener.Addr().String())
	}}
	defer transport.CloseIdleConnections()
	gate := &countedPublicGate{}
	other := newHarness(t, options{gate: gate, peers: &realtime.PeerConfig{Directory: directory, Address: netip.MustParseAddr("127.0.0.2"), Subnet: netip.MustParsePrefix("127.0.0.0/8"), HTTPClient: &http.Client{Transport: transport}}})
	ws := publicConnect(t, other, "Bearer never-forward-this", encodeCommand(t, resumeCommand("peer_reuse", "resume", 0)))
	read(t, ws)
	expectClose(t, ws, websocket.StatusPolicyViolation)
	if headers.Load() != 0 || frames.Load() != 0 || gate.admits.Load() != 0 {
		t.Fatal("unverified process received authority or caused admission")
	}
}

func TestPrivatePeerRelayDrainDoesNotEndOwnerSession(t *testing.T) {
	owner, other, _ := peerPair(t, nil)
	const id = contract.RequestID("peer_draining")
	first := owner.open(sessionRequest(id, primary))
	read(t, first)
	_ = first.CloseNow()
	second := other.connect(encodeCommand(t, resumeCommand(id, "resume", 0)), nil, nil)
	resumed := readUntil(t, second, "session.resumed")
	last := resumed[len(resumed)-1].sequence()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := other.manager.Shutdown(ctx); err != nil {
		t.Fatal("relay drain did not finish", err)
	}
	if owner.manager.Open() != 1 || len(owner.costs.recorded()) != 0 {
		t.Fatal("relay drain settled remote session")
	}
	third := owner.connect(encodeCommand(t, resumeCommand(id, "resume_again", last)), nil, nil)
	readUntil(t, third, "session.resumed")
	command(t, third, closeCommand(id, "close"))
	readUntil(t, third, "session.closed")
	read(t, third)
	expectClose(t, third, websocket.StatusNormalClosure)
	waitFor(t, "single owner settlement after resume", func() bool { return len(owner.costs.recorded()) == 1 })
	if owner.upstream.Dials() != 1 || other.upstream.Dials() != 0 {
		t.Fatal("drain replay opened second session")
	}
}

func TestPrivatePeerLeaseLossCancelsProviderAndSettlesExactlyOnce(t *testing.T) {
	gate := &countedPublicGate{}
	owner, _, directory := peerPair(t, gate)
	// Keep the provider alive beyond its test harness's usual 10s read limit,
	// so a provider timeout cannot accidentally stand in for a lost lease.
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	owner.upstream.Script = func(*fake.Conn) { <-release }
	ws := publicConnect(t, owner, "Bearer original", []byte(`{"type":"session.open"}`))
	read(t, ws)
	directory.mu.Lock()
	delete(directory.rows, "peer_public")
	directory.mu.Unlock()
	// Exercise the actual renewal interval, not an injected session cancel.
	ctx, cancel := context.WithTimeout(context.Background(), sessionowner.RenewInterval+10*time.Second)
	defer cancel()
	for {
		if _, _, err := ws.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("lost ownership did not stop session")
			}
			break
		}
	}
	waitFor(t, "lease-loss settlement", func() bool { return gate.settles.Load() == 1 && owner.manager.Open() == 0 })
	if directory.failedRenewals.Load() != 1 {
		t.Fatal("session ended without the actual failed renewal")
	}
	unblock()
	owner.upstream.WaitClosed(t)
	if gate.admits.Load() != 1 || gate.settles.Load() != 1 || owner.upstream.Dials() != 1 {
		t.Fatal("lease loss duplicated admission/settlement")
	}
}

func TestPrivatePeerShutdownClosesUnfinishedHandshake(t *testing.T) {
	owner, _, _ := peerPair(t, nil)
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, owner.server.Listener.Addr().String())
	}}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, response, err := websocket.Dial(ctx, "ws://127.0.0.1:8080"+realtime.PeerPath, &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	closeBody(response)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	if _, _, err = ws.Read(ctx); err != nil {
		t.Fatal(err)
	} // generation only; no authority sent
	drain, cancelDrain := context.WithTimeout(ctx, time.Second)
	defer cancelDrain()
	if err = owner.manager.Shutdown(drain); err != nil {
		t.Fatal("handshake survived shutdown", err)
	}
	if _, _, err = ws.Read(ctx); err == nil {
		t.Fatal("unfinished handshake still connected")
	}
	if owner.upstream.Dials() != 0 {
		t.Fatal("private hello admitted execution")
	}
}
