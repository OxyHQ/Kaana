package edgeauth_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/edgeauth"
)

const keyID = "edge-2026-08"

func newSignedRequest(t *testing.T, private ed25519.PrivateKey, id string, at time.Time, body []byte, signedBody []byte) http.Header {
	t.Helper()
	milliseconds := at.UnixMilli()
	if signedBody == nil {
		signedBody = body
	}
	signature := ed25519.Sign(private, edgeauth.SigningInput(id, milliseconds, signedBody))
	header := http.Header{}
	header.Set(edgeauth.HeaderKeyID, id)
	header.Set(edgeauth.HeaderTimestamp, strconv.FormatInt(milliseconds, 10))
	header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(signature))
	return header
}

func newVerifier(t *testing.T) (*edgeauth.Verifier, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	verifier, err := edgeauth.NewVerifier(map[string]ed25519.PublicKey{keyID: public}, time.Minute)
	if err != nil {
		t.Fatalf("building the verifier: %v", err)
	}
	return verifier, private
}

// TestAGenuineEnvelopeVerifies is the control. Every rejection below is only
// meaningful against a verifier that accepts something.
func TestAGenuineEnvelopeVerifies(t *testing.T) {
	verifier, private := newVerifier(t)
	body := []byte(`{"schemaVersion":1}`)
	if err := verifier.Verify(newSignedRequest(t, private, keyID, time.Now(), body, nil), body); err != nil {
		t.Fatalf("a genuine envelope was rejected: %v", err)
	}
}

func TestCredentialControlAndInferenceSignaturesAreNotInterchangeable(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	keys := map[string]ed25519.PublicKey{keyID: public}
	inference, err := edgeauth.NewVerifier(keys, time.Minute)
	if err != nil {
		t.Fatalf("building inference verifier: %v", err)
	}
	control, err := edgeauth.NewCredentialControlVerifier(keys, time.Minute)
	if err != nil {
		t.Fatalf("building credential-control verifier: %v", err)
	}
	body := []byte(`{"schemaVersion":1,"action":"revoke"}`)
	milliseconds := time.Now().UnixMilli()

	controlHeader := http.Header{}
	controlHeader.Set(edgeauth.HeaderKeyID, keyID)
	controlHeader.Set(edgeauth.HeaderTimestamp, strconv.FormatInt(milliseconds, 10))
	controlHeader.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(
		ed25519.Sign(private, edgeauth.CredentialControlSigningInput(keyID, milliseconds, body))))
	if err := control.Verify(controlHeader, body); err != nil {
		t.Fatalf("control signature did not verify for control: %v", err)
	}
	if err := inference.Verify(controlHeader, body); !errors.Is(err, edgeauth.ErrUnauthorized) {
		t.Fatalf("control signature verified as inference: %v", err)
	}

	inferenceHeader := newSignedRequest(t, private, keyID, time.UnixMilli(milliseconds), body, nil)
	if err := control.Verify(inferenceHeader, body); !errors.Is(err, edgeauth.ErrUnauthorized) {
		t.Fatalf("inference signature verified as credential control: %v", err)
	}
}

func TestForgeryIsRejected(t *testing.T) {
	body := []byte(`{"schemaVersion":1,"attribution":{"principal":{"billing":{"accountId":"acc_victim"}}}}`)

	cases := []struct {
		name   string
		mutate func(t *testing.T, verifier *edgeauth.Verifier, private ed25519.PrivateKey) (http.Header, []byte)
	}{
		{
			name: "a body that does not match its signature",
			mutate: func(t *testing.T, _ *edgeauth.Verifier, private ed25519.PrivateKey) (http.Header, []byte) {
				swapped := []byte(`{"schemaVersion":1,"attribution":{"principal":{"billing":{"accountId":"acc_attacker"}}}}`)
				return newSignedRequest(t, private, keyID, time.Now(), swapped, body), swapped
			},
		},
		{
			name: "a signature from a key Kaana does not trust",
			mutate: func(t *testing.T, _ *edgeauth.Verifier, _ ed25519.PrivateKey) (http.Header, []byte) {
				_, other, err := ed25519.GenerateKey(nil)
				if err != nil {
					t.Fatalf("generating a key: %v", err)
				}
				return newSignedRequest(t, other, keyID, time.Now(), body, nil), body
			},
		},
		{
			name: "a key id that is not configured",
			mutate: func(t *testing.T, _ *edgeauth.Verifier, private ed25519.PrivateKey) (http.Header, []byte) {
				return newSignedRequest(t, private, "some-other-key", time.Now(), body, nil), body
			},
		},
		{
			name: "a signature stamped too far in the past",
			mutate: func(t *testing.T, _ *edgeauth.Verifier, private ed25519.PrivateKey) (http.Header, []byte) {
				return newSignedRequest(t, private, keyID, time.Now().Add(-2*time.Hour), body, nil), body
			},
		},
		{
			// A future timestamp is not harmless: it would extend a captured
			// envelope's replay window by however far ahead it was stamped.
			name: "a signature stamped in the future",
			mutate: func(t *testing.T, _ *edgeauth.Verifier, private ed25519.PrivateKey) (http.Header, []byte) {
				return newSignedRequest(t, private, keyID, time.Now().Add(2*time.Hour), body, nil), body
			},
		},
		{
			name: "no signature at all",
			mutate: func(t *testing.T, _ *edgeauth.Verifier, _ ed25519.PrivateKey) (http.Header, []byte) {
				return http.Header{}, body
			},
		},
		{
			name: "a signature with no version prefix",
			mutate: func(t *testing.T, _ *edgeauth.Verifier, private ed25519.PrivateKey) (http.Header, []byte) {
				header := newSignedRequest(t, private, keyID, time.Now(), body, nil)
				header.Set(edgeauth.HeaderSignature, header.Get(edgeauth.HeaderSignature)[len("v1="):])
				return header, body
			},
		},
		{
			name: "a truncated signature",
			mutate: func(t *testing.T, _ *edgeauth.Verifier, private ed25519.PrivateKey) (http.Header, []byte) {
				header := newSignedRequest(t, private, keyID, time.Now(), body, nil)
				header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString([]byte("short")))
				return header, body
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			verifier, private := newVerifier(t)
			header, sent := testCase.mutate(t, verifier, private)
			err := verifier.Verify(header, sent)
			if err == nil {
				t.Fatal("accepted")
			}
			if !errors.Is(err, edgeauth.ErrUnauthorized) {
				t.Errorf("rejected with %v, which is not ErrUnauthorized", err)
			}
		})
	}
}

func TestAVerifierWithNoKeysIsRefused(t *testing.T) {
	// A verifier with no keys rejects everything, which looks exactly like a
	// misconfigured deploy and would be discovered as a total outage.
	if _, err := edgeauth.NewVerifier(nil, time.Minute); err == nil {
		t.Fatal("a verifier with no keys was built")
	}
}

func TestParsePublicKeys(t *testing.T) {
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(public)

	keys, err := edgeauth.ParsePublicKeys("a:" + encoded + " , b:" + encoded)
	if err != nil {
		t.Fatalf("parsing two keys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("parsed %d keys, expected 2", len(keys))
	}

	for _, spec := range []string{
		"",
		"missing-separator",
		"a:not-base64!!",
		"a:" + base64.StdEncoding.EncodeToString([]byte("too short")),
		"a:" + encoded + ",a:" + encoded,
	} {
		if _, err := edgeauth.ParsePublicKeys(spec); err == nil {
			t.Errorf("accepted the key specification %q", spec)
		}
	}
}

func TestAnotherHeaderNamespaceIsNotAccepted(t *testing.T) {
	verifier, private := newVerifier(t)
	body := []byte(`{"schemaVersion":1}`)
	current := newSignedRequest(t, private, keyID, time.Now(), body, nil)

	other := http.Header{}
	other.Set("X-Oxy-Gateway-Key-Id", current.Get(edgeauth.HeaderKeyID))
	other.Set("X-Oxy-Gateway-Timestamp", current.Get(edgeauth.HeaderTimestamp))
	other.Set("X-Oxy-Gateway-Signature", current.Get(edgeauth.HeaderSignature))

	if err := verifier.Verify(other, body); err == nil {
		t.Fatal("a header spelling this service never used was accepted")
	}
}
