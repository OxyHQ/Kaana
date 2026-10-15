package publicapi

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// Realtime subprotocols (`@kaana.ai/contracts` 4.1.0).
const (
	RealtimeSubprotocol        = "kaana.v1"
	realtimeBearerPrefix       = "kaana.bearer."
	realtimeOnBehalfOfPrefix   = "kaana.user."
	maxCredentialBytes         = 8192
	maxOnBehalfOfUserIDLength  = 64
	maxIdempotencyKeyLength    = 255
	maxClientRequestIDLength   = 128
	credentialDigestCacheLimit = 100_000
)

// credential is the caller's bearer exactly as presented. Kaana forwards it to
// Oxy and never stores, logs or interprets it; its digest keys the local rate
// limit and binds a realtime resume to the credential that opened the session.
type credential struct {
	value  string
	digest [32]byte
}

// bearerFrom reads the credential from Authorization, or, for a WebSocket
// whose runtime cannot set headers, from the kaana.bearer. subprotocol.
func bearerFrom(r *http.Request) (credential, bool) {
	if header := r.Header.Get("Authorization"); header != "" {
		scheme, value, found := strings.Cut(header, " ")
		if !found || !strings.EqualFold(scheme, "Bearer") {
			return credential{}, false
		}
		return newCredential(strings.TrimSpace(value))
	}
	for _, protocol := range offeredSubprotocols(r) {
		if encoded, found := strings.CutPrefix(protocol, realtimeBearerPrefix); found {
			decoded, err := base64.RawURLEncoding.DecodeString(encoded)
			if err != nil {
				return credential{}, false
			}
			return newCredential(string(decoded))
		}
	}
	return credential{}, false
}

func newCredential(value string) (credential, bool) {
	if value == "" || len(value) > maxCredentialBytes || strings.ContainsAny(value, " \t\r\n") {
		return credential{}, false
	}
	return credential{value: value, digest: sha256.Sum256([]byte(value))}, true
}

// onBehalfOf reads X-Oxy-User-Id, or the kaana.user. subprotocol. Oxy honours
// it only with a service token; Kaana only bounds it.
func onBehalfOf(r *http.Request) (*contract.UserID, bool) {
	value := r.Header.Get(HeaderOnBehalfOf)
	if value == "" {
		for _, protocol := range offeredSubprotocols(r) {
			if encoded, found := strings.CutPrefix(protocol, realtimeOnBehalfOfPrefix); found {
				decoded, err := base64.RawURLEncoding.DecodeString(encoded)
				if err != nil {
					return nil, false
				}
				value = string(decoded)
			}
		}
	}
	if value == "" {
		return nil, true
	}
	if len(value) > maxOnBehalfOfUserIDLength || strings.ContainsAny(value, " \t\r\n") {
		return nil, false
	}
	user := contract.UserID(value)
	return &user, true
}

// idempotencyKey reads Idempotency-Key.
func idempotencyKey(r *http.Request) (*contract.IdempotencyKey, bool) {
	value := r.Header.Get(HeaderIdempotencyKey)
	if value == "" {
		return nil, true
	}
	if len(value) > maxIdempotencyKeyLength || strings.ContainsAny(value, "\r\n") {
		return nil, false
	}
	key := contract.IdempotencyKey(value)
	return &key, true
}

func offeredSubprotocols(r *http.Request) []string {
	var protocols []string
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, protocol := range strings.Split(header, ",") {
			if protocol = strings.TrimSpace(protocol); protocol != "" {
				protocols = append(protocols, protocol)
			}
		}
	}
	return protocols
}

/* -------------------------------------------------------------------------- */
/*  Local rate limit                                                          */
/* -------------------------------------------------------------------------- */

// RateLimit bounds the requests one credential may make per second, before
// Oxy is asked for anything. Oxy enforces the customer's own limits in the
// authorization; this one keeps a single credential (or a leaked one) from
// turning Kaana into an amplifier against Oxy's authorization endpoint.
type RateLimit struct {
	PerSecond float64
	Burst     int
}

// DefaultRateLimit is generous enough for any legitimate client and tight
// enough that one credential cannot flood the authorization path.
var DefaultRateLimit = RateLimit{PerSecond: 50, Burst: 100}

type bucket struct {
	tokens float64
	at     time.Time
}

type limiter struct {
	rate RateLimit
	now  func() time.Time

	mu      sync.Mutex
	buckets map[[32]byte]*bucket
}

func newLimiter(rate RateLimit, now func() time.Time) *limiter {
	return &limiter{rate: rate, now: now, buckets: make(map[[32]byte]*bucket)}
}

// allow takes one token for the credential, reporting how long to wait when
// there is none.
func (l *limiter) allow(digest [32]byte) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	current, known := l.buckets[digest]
	if !known {
		if len(l.buckets) >= credentialDigestCacheLimit {
			// Bounded memory: forget idle buckets rather than grow without
			// limit under a spray of distinct credentials.
			for key, idle := range l.buckets {
				if now.Sub(idle.at) > time.Minute {
					delete(l.buckets, key)
				}
			}
		}
		current = &bucket{tokens: float64(l.rate.Burst), at: now}
		l.buckets[digest] = current
	}
	current.tokens = min(float64(l.rate.Burst), current.tokens+now.Sub(current.at).Seconds()*l.rate.PerSecond)
	current.at = now
	if current.tokens >= 1 {
		current.tokens--
		return 0, true
	}
	wait := time.Duration((1 - current.tokens) / l.rate.PerSecond * float64(time.Second))
	return wait, false
}
