package realtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/sessionowner"
	"github.com/coder/websocket"
)

const PeerPath = "/internal/peer/realtime-resume"
const peerPort = "8080"
const peerTimeout = 3 * time.Second
const maxPeerFrame = 32 << 10

// PeerConfig is infrastructure placement, never a caller-provided URL. The
// runtime derives Address from its own ECS container metadata and VPC CIDR.
type PeerConfig struct {
	Directory  sessionowner.Directory
	Address    netip.Addr
	Subnet     netip.Prefix
	Generation string
	HTTPClient *http.Client
}

type peerState struct{ PeerConfig }

type peerResume struct {
	Lane   string      `json:"lane"`
	Header http.Header `json:"header"`
	Frame  []byte      `json:"frame"`
}

func newPeers(config *PeerConfig) (*peerState, error) {
	if config == nil {
		return nil, nil
	}
	if config.Directory == nil || !config.Address.Is4() || !config.Subnet.IsValid() || !config.Subnet.Contains(config.Address) ||
		(!config.Subnet.Addr().IsPrivate() && !config.Subnet.Addr().IsLoopback()) || config.Subnet.Bits() < 8 {
		return nil, errors.New("realtime peer requires its private address, exact VPC subnet and directory")
	}
	value := *config
	if value.Generation == "" {
		var bytes [16]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			return nil, err
		}
		value.Generation = hex.EncodeToString(bytes[:])
	}
	decoded, err := hex.DecodeString(value.Generation)
	if err != nil || len(decoded) != 16 || strings.ToLower(value.Generation) != value.Generation {
		return nil, errors.New("invalid realtime process generation")
	}
	// Private placement traffic must not use an environment HTTP proxy.
	client := http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: peerTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: peerTimeout,
		IdleConnTimeout:       30 * time.Second,
	}}
	if value.HTTPClient != nil {
		client = *value.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("realtime peer redirects are refused") }
	value.HTTPClient = &client
	return &peerState{value}, nil
}

func (m *Manager) ownerEntry(id contract.RequestID, public bool) sessionowner.Entry {
	lane := sessionowner.Internal
	if public {
		lane = sessionowner.Public
	}
	return sessionowner.Entry{RequestID: string(id), Lane: lane, Address: m.peers.Address.String(), Generation: m.peers.Generation}
}
func (m *Manager) claimOwner(ctx context.Context, s *session) bool {
	if m.peers == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, peerTimeout)
	defer cancel()
	ok, err := m.peers.Directory.Claim(ctx, m.ownerEntry(s.requestID, s.public))
	return err == nil && ok
}
func (m *Manager) renewOwner(ctx context.Context, s *session) {
	if m.peers == nil {
		return
	}
	ticker := time.NewTicker(sessionowner.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			attempt, cancel := context.WithTimeout(ctx, peerTimeout)
			ok, err := m.peers.Directory.Renew(attempt, m.ownerEntry(s.requestID, s.public))
			cancel()
			if err != nil || !ok {
				s.cancel()
				return
			}
		}
	}
}
func (m *Manager) releaseOwner(parent context.Context, s *session) {
	if m.peers == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), peerTimeout)
	defer cancel()
	if err := m.peers.Directory.Release(ctx, m.ownerEntry(s.requestID, s.public)); err != nil {
		m.logger.Warn("realtime owner release failed; lease will expire")
	}
}

func peerHeaders(header http.Header, lane string) http.Header {
	result := make(http.Header)
	names := []string{"Authorization", "Sec-WebSocket-Protocol", "Origin"}
	if lane == sessionowner.Internal {
		names = []string{edgeauth.HeaderKeyID, edgeauth.HeaderTimestamp, edgeauth.HeaderSignature}
	}
	for _, name := range names {
		for _, v := range header.Values(name) {
			result.Add(name, v)
		}
	}
	return result
}

// forwardResume is one hop only. It sends no credential before the connected
// process proves the exact generation read from the short-lived directory.
func (m *Manager) forwardResume(r *http.Request, client *websocket.Conn, frame []byte, lane string) bool {
	if m.peers == nil {
		return false
	}
	m.mu.Lock()
	if m.draining {
		m.mu.Unlock()
		return false
	}
	m.running.Add(1)
	m.mu.Unlock()
	defer m.running.Done()
	var probe struct {
		RequestID contract.RequestID `json:"requestId"`
	}
	if json.Unmarshal(frame, &probe) != nil {
		return false
	}
	decoded, err := contract.DecodeRealtimeCommand(frame, probe.RequestID)
	if _, ok := decoded.(*contract.RealtimeSessionResumeCommand); err != nil || !ok {
		return false
	}
	m.mu.Lock()
	local := m.sessions[probe.RequestID] != nil
	m.mu.Unlock()
	if local {
		return false
	}
	lookup, cancel := context.WithTimeout(r.Context(), peerTimeout)
	owner, found, err := m.peers.Directory.Lookup(lookup, string(probe.RequestID), lane)
	cancel()
	address, parseErr := netip.ParseAddr(owner.Address)
	if err != nil || !found || parseErr != nil || !address.Is4() || !m.peers.Subnet.Contains(address) ||
		owner.Lane != lane || owner.RequestID != string(probe.RequestID) || !owner.ExpiresAt.After(m.now()) ||
		(address == m.peers.Address && owner.Generation == m.peers.Generation) {
		return false
	}
	handshake, cancel := context.WithTimeout(r.Context(), peerTimeout)
	defer cancel()
	peer, response, err := websocket.Dial(handshake, "ws://"+net.JoinHostPort(address.String(), peerPort)+PeerPath, &websocket.DialOptions{HTTPClient: m.peers.HTTPClient})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return false
	}
	peer.SetReadLimit(maxPeerFrame)
	kind, hello, err := peer.Read(handshake)
	var generation struct {
		Generation string `json:"generation"`
	}
	if err != nil || kind != websocket.MessageText || json.Unmarshal(hello, &generation) != nil || generation.Generation != owner.Generation {
		_ = peer.CloseNow()
		return false
	}
	message, err := json.Marshal(peerResume{Lane: lane, Header: peerHeaders(r.Header, lane), Frame: frame})
	if err != nil || len(message) > maxPeerFrame || peer.Write(handshake, websocket.MessageText, message) != nil {
		_ = peer.CloseNow()
		return false
	}
	peer.SetReadLimit(maxCommandBytes)
	client.SetReadLimit(maxCommandBytes)
	cancel()
	m.relay(r.Context(), client, peer)
	return true
}

// PeerHandler accepts only private direct connections addressed to this exact
// process. ALB traffic carries forwarded headers and is refused before upgrade.
// The peer can resume a local session; it cannot create or forward one.
func (m *Manager) PeerHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.peers == nil {
			http.NotFound(w, r)
			return
		}
		remote, _, err := net.SplitHostPort(r.RemoteAddr)
		address, parseErr := netip.ParseAddr(remote)
		if err != nil || parseErr != nil || !m.peers.Subnet.Contains(address) || r.Host != net.JoinHostPort(m.peers.Address.String(), peerPort) || r.URL.RawQuery != "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		for key := range r.Header {
			if strings.EqualFold(key, "Forwarded") || strings.EqualFold(key, "cf-connecting-ip") || strings.HasPrefix(strings.ToLower(key), "x-forwarded-") {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
		}
		m.mu.Lock()
		draining := m.draining
		gate := m.publicGate
		if !draining {
			m.running.Add(1)
		}
		m.mu.Unlock()
		if draining {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		defer m.running.Done()
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		ws.SetReadLimit(maxPeerFrame)
		ctx, cancel := context.WithTimeout(r.Context(), peerTimeout)
		defer cancel()
		go func() {
			select {
			case <-m.shutdown:
				cancel()
				_ = ws.CloseNow()
			case <-ctx.Done():
			}
		}()
		hello, _ := json.Marshal(struct {
			Generation string `json:"generation"`
		}{m.peers.Generation})
		if ws.Write(ctx, websocket.MessageText, hello) != nil {
			_ = ws.CloseNow()
			return
		}
		kind, raw, err := ws.Read(ctx)
		var message peerResume
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if err != nil || kind != websocket.MessageText || decoder.Decode(&message) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
			(message.Lane != sessionowner.Internal && message.Lane != sessionowner.Public) || len(message.Frame) > MaxFirstFrameBytes {
			_ = ws.Close(websocket.StatusPolicyViolation, "")
			return
		}
		var probe struct {
			RequestID contract.RequestID `json:"requestId"`
		}
		_ = json.Unmarshal(message.Frame, &probe)
		decoded, err := contract.DecodeRealtimeCommand(message.Frame, probe.RequestID)
		if _, ok := decoded.(*contract.RealtimeSessionResumeCommand); err != nil || !ok {
			_ = ws.Close(websocket.StatusPolicyViolation, "")
			return
		}
		owner, found, err := m.peers.Directory.Lookup(ctx, string(probe.RequestID), message.Lane)
		if err != nil || !found || owner.Generation != m.peers.Generation || owner.Address != m.peers.Address.String() || !owner.ExpiresAt.After(m.now()) {
			_ = ws.Close(websocket.StatusPolicyViolation, "")
			return
		}
		original := r.Clone(r.Context())
		original.Header = peerHeaders(message.Header, message.Lane)
		if message.Lane == sessionowner.Internal {
			if m.verifier.Verify(original.Header, message.Frame) != nil {
				_ = ws.Close(websocket.StatusPolicyViolation, "")
				return
			}
			m.resume(r.Context(), ws, message.Frame)
		} else {
			if gate == nil {
				_ = ws.Close(websocket.StatusPolicyViolation, "")
				return
			}
			// This private path deliberately calls the local-only resume implementation.
			m.publicResumeLocal(original, ws, message.Frame, gate)
		}
	})
}

func (m *Manager) relay(ctx context.Context, left, right *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() { _ = left.CloseNow() }()
	defer func() { _ = right.CloseNow() }()
	// The owner pings its private connection. This hop must independently keep
	// the client's ALB connection alive, including sessions with no audio yet.
	go keepAlive(ctx, left, m.pingInterval)
	go func() {
		select {
		case <-m.shutdown:
			cancel()
			_ = left.CloseNow()
			_ = right.CloseNow()
		case <-ctx.Done():
		}
	}()
	done := make(chan error, 2)
	copyFrames := func(from, to *websocket.Conn) {
		for {
			kind, data, err := from.Read(ctx)
			if err != nil {
				done <- err
				return
			}
			write, cancelWrite := context.WithTimeout(ctx, m.writeTimeout)
			err = to.Write(write, kind, data)
			cancelWrite()
			if err != nil {
				done <- err
				return
			}
		}
	}
	go copyFrames(left, right)
	go copyFrames(right, left)
	err := <-done
	status := websocket.CloseStatus(err)
	if status >= 1000 && status != websocket.StatusAbnormalClosure && status != websocket.StatusNoStatusRcvd {
		_ = left.Close(status, "")
		_ = right.Close(status, "")
	}
	cancel()
	_ = left.CloseNow()
	_ = right.CloseNow()
	<-done
}
