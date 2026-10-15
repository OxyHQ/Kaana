// Package edgeauth verifies that an envelope came from the Oxy edge.
//
// # Why this is asymmetric
//
// The published contract specifies the SHAPES Oxy and Kaana exchange and says
// nothing about how Kaana authenticates the sender. Something has to be chosen,
// and the choice follows Oxy's own ADR 0012 rather than inventing a position:
// that ADR retires the shared HMAC secret for service tokens on the ground that
// in any symmetric scheme, verifying and minting are the same capability — so
// the key that lets a verifier READ an attribution is the key that lets it
// FORGE one.
//
// Applied to this hop, the reasoning is the same and the stakes are higher.
// Kaana executes requests and reports the usage a customer is charged for. A
// shared HMAC secret would let Kaana — or anything that ever read Kaana's
// configuration — mint an envelope naming any Oxy account as the payer, and a
// forged accountId is indistinguishable from a real one at every point after
// the mint. So Kaana holds only PUBLIC keys and cannot construct an envelope it
// would itself accept.
//
// This is Kaana's proposal for a decision Oxy has not made. It is stated here,
// in one file, so it is cheap to replace if Oxy decides otherwise.
//
// # The scheme
//
//	X-Oxy-Kaana-Key-Id       the signing key's id
//	X-Oxy-Kaana-Timestamp    unix milliseconds, when the edge signed
//	X-Oxy-Kaana-Signature    v1=<base64 Ed25519 signature>
//
// signed over, with `\n` separators and no trailing newline:
//
//	oxy-kaana-envelope:v1
//	<key id>
//	<timestamp>
//	<lowercase hex sha256 of the exact request body>
//
// The domain prefix means a signature minted for any other Oxy purpose cannot be
// replayed here, and the body hash means the signature covers the envelope
// rather than merely accompanying it.
package edgeauth

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// Header names. They are Kaana's own, so they are namespaced rather than
// generic: a proxy that strips or rewrites `Authorization` must not be able to
// affect this.
const (
	HeaderKeyID     = "X-Oxy-Kaana-Key-Id"
	HeaderTimestamp = "X-Oxy-Kaana-Timestamp"
	HeaderSignature = "X-Oxy-Kaana-Signature"
)

const signaturePrefix = "v1="

const (
	// inferenceDomainSeparator keeps a signature minted for another Oxy purpose
	// from being replayable as an inference envelope.
	inferenceDomainSeparator = "oxy-kaana-envelope:v1"
	// credentialControlDomainSeparator also separates BYOK mutation authority
	// from inference authority if an operator accidentally deploys the same key
	// pair to both tasks.
	credentialControlDomainSeparator = "oxy-kaana-credential-control:v1"
	// platformCredentialControlDomainSeparator isolates the authority that may
	// create Kaana-owned provider pool credentials from customer BYOK mutation.
	platformCredentialControlDomainSeparator = "oxy-kaana-platform-credential-control:v1"
	// credentialValidationDomainSeparator prevents a bootstrap probe from being
	// replayed as normal inference even when both surfaces trust the same Oxy
	// signing key.
	credentialValidationDomainSeparator = "oxy-kaana-credential-validation:v1"
	// providerTelemetryDomainSeparator scopes the operator-only telemetry read:
	// an inference signature cannot read upstream costs, and a telemetry read
	// signature cannot run inference.
	providerTelemetryDomainSeparator = "oxy-kaana-provider-telemetry:v1"
	// grantDomainSeparator is the domain of a grant Oxy signs in answer to an
	// authorization (Kaana ADR 0002, decision 3): the same Oxy keys, a purpose
	// no envelope or telemetry signature can be replayed as.
	grantDomainSeparator = contract.GrantSignatureDomain
	// settlementReadDomainSeparator scopes Oxy's reconciliation read of
	// Kaana's settlement outbox (GET /internal/v1/settlements). The signed
	// bytes are the raw query string, which is the whole request.
	settlementReadDomainSeparator = contract.SettlementReadSignatureDomain
	// settlementDomainSeparator is the domain of a settlement KAANA signs with
	// its own key for Oxy; Kaana never verifies under it.
	settlementDomainSeparator = contract.SettlementSignatureDomain
)

// DefaultMaxSkew bounds how far a signature's timestamp may be from now.
//
// It is the only replay bound: Kaana keeps no nonce cache, so a captured
// envelope can be replayed within this window. That is acceptable only because
// the edge owns request idempotency and spend reservation — a replayed envelope
// spends against a reservation that has already been made. It is recorded here
// rather than assumed, because the assumption is what would make it wrong.
const DefaultMaxSkew = 5 * time.Minute

// Verifier checks signatures against a set of public keys.
type Verifier struct {
	keys    map[string]ed25519.PublicKey
	maxSkew time.Duration
	now     func() time.Time
	domain  string
}

// ErrUnauthorized is returned for every verification failure.
//
// One error for every cause on purpose: an unknown key id, a bad signature and
// a stale timestamp are all "this did not come from the Oxy edge", and
// distinguishing them for the caller only tells an attacker which half of a
// forgery attempt was closer.
var ErrUnauthorized = errors.New("edgeauth: the request is not a signed Oxy edge envelope")

// NewVerifier builds a verifier. It refuses an empty key set: a verifier with
// no keys rejects everything, which looks exactly like a misconfigured deploy
// and would be discovered as a total outage.
func NewVerifier(keys map[string]ed25519.PublicKey, maxSkew time.Duration) (*Verifier, error) {
	return newVerifier(keys, maxSkew, inferenceDomainSeparator)
}

// NewCredentialControlVerifier builds the separate BYOK control verifier.
// Its signatures cannot authorize inference, and inference signatures cannot
// authorize a credential mutation or outcome query, even if a key pair is
// misconfigured in both public-key sets.
func NewCredentialControlVerifier(keys map[string]ed25519.PublicKey, maxSkew time.Duration) (*Verifier, error) {
	return newVerifier(keys, maxSkew, credentialControlDomainSeparator)
}

// NewPlatformCredentialControlVerifier builds the verifier used only for
// platform provider-pool credential imports.
func NewPlatformCredentialControlVerifier(keys map[string]ed25519.PublicKey, maxSkew time.Duration) (*Verifier, error) {
	return newVerifier(keys, maxSkew, platformCredentialControlDomainSeparator)
}

// NewCredentialValidationVerifier builds the dedicated pending-generation
// bootstrap verifier. It trusts the same public key material as the edge while
// assigning that key a distinct signed purpose.
func NewCredentialValidationVerifier(keys map[string]ed25519.PublicKey, maxSkew time.Duration) (*Verifier, error) {
	return newVerifier(keys, maxSkew, credentialValidationDomainSeparator)
}

// NewProviderTelemetryVerifier builds the verifier for Oxy's read of Kaana's
// operator telemetry: attempt costs and credential economics.
func NewProviderTelemetryVerifier(keys map[string]ed25519.PublicKey, maxSkew time.Duration) (*Verifier, error) {
	return newVerifier(keys, maxSkew, providerTelemetryDomainSeparator)
}

// NewGrantVerifier builds the verifier for the grants Oxy signs in answer to
// an authorization. Same public keys as the envelope, a different domain.
func NewGrantVerifier(keys map[string]ed25519.PublicKey, maxSkew time.Duration) (*Verifier, error) {
	return newVerifier(keys, maxSkew, grantDomainSeparator)
}

// NewSettlementReadVerifier builds the verifier for Oxy's reconciliation read
// of the settlement outbox.
func NewSettlementReadVerifier(keys map[string]ed25519.PublicKey, maxSkew time.Duration) (*Verifier, error) {
	return newVerifier(keys, maxSkew, settlementReadDomainSeparator)
}

func newVerifier(keys map[string]ed25519.PublicKey, maxSkew time.Duration, domain string) (*Verifier, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("edgeauth: no edge public keys are configured, so no envelope could ever be accepted")
	}
	for id, key := range keys {
		if id == "" {
			return nil, fmt.Errorf("edgeauth: a public key has no id, so no signature could name it")
		}
		if len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("edgeauth: key %q is %d bytes, an Ed25519 public key is %d", id, len(key), ed25519.PublicKeySize)
		}
	}
	if maxSkew <= 0 {
		maxSkew = DefaultMaxSkew
	}
	return &Verifier{keys: keys, maxSkew: maxSkew, now: time.Now, domain: domain}, nil
}

// Verify checks the signature over body. The caller must pass the EXACT bytes
// it will parse: verifying a re-encoded body would authenticate something other
// than what gets executed.
func (v *Verifier) Verify(header http.Header, body []byte) error {
	keyID := header.Get(HeaderKeyID)
	key, known := v.keys[keyID]
	if !known {
		return fmt.Errorf("%w: unknown key id", ErrUnauthorized)
	}

	milliseconds, err := strconv.ParseInt(header.Get(HeaderTimestamp), 10, 64)
	if err != nil {
		return fmt.Errorf("%w: unreadable timestamp", ErrUnauthorized)
	}
	signedAt := time.UnixMilli(milliseconds)
	// Both directions. A future timestamp is not harmless: it would extend a
	// captured envelope's replay window by however far ahead it was stamped.
	if drift := v.now().Sub(signedAt); drift > v.maxSkew || drift < -v.maxSkew {
		return fmt.Errorf("%w: the signature is outside the accepted time window", ErrUnauthorized)
	}

	raw := header.Get(HeaderSignature)
	if !strings.HasPrefix(raw, signaturePrefix) {
		return fmt.Errorf("%w: unversioned signature", ErrUnauthorized)
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, signaturePrefix))
	if err != nil || len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("%w: malformed signature", ErrUnauthorized)
	}

	if !ed25519.Verify(key, signingInput(v.domain, keyID, milliseconds, body), signature) {
		return fmt.Errorf("%w: signature does not verify", ErrUnauthorized)
	}
	return nil
}

// SigningInput builds the exact bytes both sides sign.
//
// Exported because it IS the specification: an Oxy-side implementation that
// reads this function cannot get the framing subtly wrong, and Kaana's own
// tests sign with it rather than with a second copy that could drift.
func SigningInput(keyID string, timestampMillis int64, body []byte) []byte {
	return signingInput(inferenceDomainSeparator, keyID, timestampMillis, body)
}

// CredentialControlSigningInput is the exact, separately domain-bound input
// Oxy signs for a customer provider credential mutation or outcome query.
func CredentialControlSigningInput(keyID string, timestampMillis int64, body []byte) []byte {
	return signingInput(credentialControlDomainSeparator, keyID, timestampMillis, body)
}

// PlatformCredentialControlSigningInput is the exact input an operator-side
// client signs for one Kaana platform provider credential mutation.
func PlatformCredentialControlSigningInput(keyID string, timestampMillis int64, body []byte) []byte {
	return signingInput(platformCredentialControlDomainSeparator, keyID, timestampMillis, body)
}

// CredentialValidationSigningInput is the exact input Oxy signs for one BYOK
// bootstrap operation.
func CredentialValidationSigningInput(keyID string, timestampMillis int64, body []byte) []byte {
	return signingInput(credentialValidationDomainSeparator, keyID, timestampMillis, body)
}

// ProviderTelemetrySigningInput is the exact input Oxy signs to read operator
// telemetry.
func ProviderTelemetrySigningInput(keyID string, timestampMillis int64, body []byte) []byte {
	return signingInput(providerTelemetryDomainSeparator, keyID, timestampMillis, body)
}

// GrantSigningInput is the exact input Oxy signs for a grant body.
func GrantSigningInput(keyID string, timestampMillis int64, body []byte) []byte {
	return signingInput(grantDomainSeparator, keyID, timestampMillis, body)
}

// SettlementReadSigningInput is the exact input Oxy signs to read the
// settlement outbox; body is the raw query string.
func SettlementReadSigningInput(keyID string, timestampMillis int64, rawQuery []byte) []byte {
	return signingInput(settlementReadDomainSeparator, keyID, timestampMillis, rawQuery)
}

// SettlementSigningInput is the exact input Kaana signs for a settlement body,
// published so Oxy's verifier cannot frame it differently.
func SettlementSigningInput(keyID string, timestampMillis int64, body []byte) []byte {
	return signingInput(settlementDomainSeparator, keyID, timestampMillis, body)
}

// Signer signs Kaana's own messages to Oxy under one domain with one private
// key. It is the inverse of Verifier: Kaana holds the private half, Oxy the
// public one, so Oxy can verify a settlement and cannot mint one.
type Signer struct {
	keyID  string
	key    ed25519.PrivateKey
	domain string
	now    func() time.Time
}

// NewSettlementSigner builds the signer of Kaana's settlements from the
// `<key-id>:<base64 Ed25519 seed>` configuration form.
func NewSettlementSigner(spec string) (*Signer, error) {
	id, encoded, found := strings.Cut(strings.TrimSpace(spec), ":")
	id = strings.TrimSpace(id)
	if !found || id == "" {
		return nil, errors.New("edgeauth: the settlement signing key is not <key-id>:<base64 seed>")
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(seed) != ed25519.SeedSize {
		// The value is a private key: neither it nor the decoder's error,
		// which can quote it, is repeated.
		return nil, fmt.Errorf("edgeauth: the settlement signing key %q is not a base64 %d-byte Ed25519 seed", id, ed25519.SeedSize)
	}
	key := ed25519.NewKeyFromSeed(seed)
	clear(seed)
	return &Signer{keyID: id, key: key, domain: settlementDomainSeparator, now: time.Now}, nil
}

// KeyID is the id Oxy looks the public key up by.
func (s *Signer) KeyID() string { return s.keyID }

// PublicKey is the half Oxy stores; logging it is harmless.
func (s *Signer) PublicKey() ed25519.PublicKey { return s.key.Public().(ed25519.PublicKey) }

// Sign stamps the three signature headers over body onto header.
func (s *Signer) Sign(header http.Header, body []byte) {
	timestamp := s.now().UnixMilli()
	signature := ed25519.Sign(s.key, signingInput(s.domain, s.keyID, timestamp, body))
	header.Set(HeaderKeyID, s.keyID)
	header.Set(HeaderTimestamp, strconv.FormatInt(timestamp, 10))
	header.Set(HeaderSignature, signaturePrefix+base64.StdEncoding.EncodeToString(signature))
}

func signingInput(domain, keyID string, timestampMillis int64, body []byte) []byte {
	digest := sha256.Sum256(body)
	return []byte(strings.Join([]string{
		domain,
		keyID,
		strconv.FormatInt(timestampMillis, 10),
		hex.EncodeToString(digest[:]),
	}, "\n"))
}

// ParsePublicKeys reads the `kid:base64,kid:base64` configuration form.
//
// Public keys are not secrets, so this value is ordinary configuration and may
// appear in a task definition, a workflow file or a log. The corresponding
// private key exists only in Oxy.
func ParsePublicKeys(spec string) (map[string]ed25519.PublicKey, error) {
	keys := make(map[string]ed25519.PublicKey)
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, encoded, found := strings.Cut(entry, ":")
		if !found {
			return nil, fmt.Errorf("edgeauth: %q is not <key-id>:<base64 public key>", entry)
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return nil, fmt.Errorf("edgeauth: key %q is not base64: %w", id, err)
		}
		if len(decoded) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("edgeauth: key %q decodes to %d bytes, an Ed25519 public key is %d", id, len(decoded), ed25519.PublicKeySize)
		}
		if _, duplicate := keys[strings.TrimSpace(id)]; duplicate {
			return nil, fmt.Errorf("edgeauth: key id %q appears twice", id)
		}
		keys[strings.TrimSpace(id)] = ed25519.PublicKey(decoded)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("edgeauth: no keys were parsed from the configuration")
	}
	return keys, nil
}

// KeyIDs lists the configured key ids, so an operator can confirm which keys a
// running process trusts without being shown key material.
func (v *Verifier) KeyIDs() []string {
	ids := make([]string, 0, len(v.keys))
	for id := range v.keys {
		ids = append(ids, id)
	}
	return ids
}
