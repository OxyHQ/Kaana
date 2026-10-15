package edgeauth_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/edgeauth"
)

func signWith(input func(string, int64, []byte) []byte, private ed25519.PrivateKey, body []byte) http.Header {
	milliseconds := time.Now().UnixMilli()
	header := http.Header{}
	header.Set(edgeauth.HeaderKeyID, keyID)
	header.Set(edgeauth.HeaderTimestamp, strconv.FormatInt(milliseconds, 10))
	header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(ed25519.Sign(private, input(keyID, milliseconds, body))))
	return header
}

// A grant is verified under its own domain, and neither an envelope nor a
// telemetry signature over the same bytes passes as one (nor the reverse).
func TestGrantSignaturesAreTheirOwnPurpose(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{keyID: public}
	grants, err := edgeauth.NewGrantVerifier(keys, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	envelopes, err := edgeauth.NewVerifier(keys, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"schemaVersion":1,"grantId":"grt_1"}`)
	if err := grants.Verify(signWith(edgeauth.GrantSigningInput, private, body), body); err != nil {
		t.Fatalf("a genuine grant was rejected: %v", err)
	}
	if grants.Verify(signWith(edgeauth.SigningInput, private, body), body) == nil {
		t.Fatal("an envelope signature passed as a grant")
	}
	if grants.Verify(signWith(edgeauth.ProviderTelemetrySigningInput, private, body), body) == nil {
		t.Fatal("a telemetry signature passed as a grant")
	}
	if envelopes.Verify(signWith(edgeauth.GrantSigningInput, private, body), body) == nil {
		t.Fatal("a grant signature passed as an inference envelope")
	}
	if grants.Verify(signWith(edgeauth.GrantSigningInput, private, body), []byte(`{"schemaVersion":1,"grantId":"grt_2"}`)) == nil {
		t.Fatal("a grant signature covered other bytes")
	}
}

// Kaana signs settlements with its own key, which Oxy verifies under the
// settlement domain: the public half reproduces the signature and only that
// domain does.
func TestSettlementsAreSignedUnderKaanasOwnKey(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(index)
	}
	signer, err := edgeauth.NewSettlementSigner("kaana-settlement-2026-10:" + base64.StdEncoding.EncodeToString(seed))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"schemaVersion":1,"settlementId":"stl_1"}`)
	header := http.Header{}
	signer.Sign(header, body)
	if header.Get(edgeauth.HeaderKeyID) != "kaana-settlement-2026-10" {
		t.Fatalf("signed under key %q", header.Get(edgeauth.HeaderKeyID))
	}
	milliseconds, err := strconv.ParseInt(header.Get(edgeauth.HeaderTimestamp), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.StdEncoding.DecodeString(header.Get(edgeauth.HeaderSignature)[len("v1="):])
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(signer.PublicKey(), edgeauth.SettlementSigningInput(signer.KeyID(), milliseconds, body), signature) {
		t.Fatal("the settlement signature does not verify under the settlement domain")
	}
	if ed25519.Verify(signer.PublicKey(), edgeauth.GrantSigningInput(signer.KeyID(), milliseconds, body), signature) {
		t.Fatal("a settlement signature verifies as a grant")
	}
	for _, malformed := range []string{"", "no-colon", "id:not-base64!", "id:" + base64.StdEncoding.EncodeToString(seed[:16]), ":" + base64.StdEncoding.EncodeToString(seed)} {
		if _, err := edgeauth.NewSettlementSigner(malformed); err == nil {
			t.Errorf("a malformed signing key %q was accepted", malformed)
		}
	}
}
