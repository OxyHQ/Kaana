package awssig

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// AWS publishes a signing test suite; `get-vanilla` is its simplest case and
// pins every part of the algorithm at once — the canonical request, the scope,
// the key derivation and the rendering of the header.
//
// It is here rather than a second reading of the specification by the same
// author because a self-written expectation would agree with a self-written
// implementation about any mistake they share, which is exactly the class of
// mistake a signing algorithm fails with.
const (
	vectorAccessKeyID     = "AKIDEXAMPLE"
	vectorSecretAccessKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	vectorRegion          = "us-east-1"
	vectorService         = "service"
	vectorHost            = "example.amazonaws.com"
	vectorSignature       = "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
)

func vectorTime(t *testing.T) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, "2015-08-30T12:36:00Z")
	if err != nil {
		t.Fatalf("parsing the vector's instant: %v", err)
	}
	return at
}

// TestSignatureMatchesTheAWSGetVanillaVector is the known-answer test. The
// vector signs no payload, so its hashed payload is the sha256 of the empty
// string, and it signs exactly `host` and `x-amz-date`.
func TestSignatureMatchesTheAWSGetVanillaVector(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://"+vectorHost+"/", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}

	// The vector's signed header list is `host;x-amz-date` — it predates the
	// S3-only content hash header — so the test reaches the algorithm through
	// `sign`, which signs exactly the headers already present. Calling the
	// exported Sign would add `x-amz-content-sha256` and could not reproduce
	// the published signature.
	if err := sign(request, HashPayload(nil), Credentials{
		AccessKeyID:     vectorAccessKeyID,
		SecretAccessKey: vectorSecretAccessKey,
	}, vectorRegion, vectorService, vectorTime(t)); err != nil {
		t.Fatalf("signing: %v", err)
	}
	signed := request.Header.Get("Authorization")

	if !strings.Contains(signed, "Signature="+vectorSignature) {
		t.Errorf("signature does not match AWS's get-vanilla vector.\n got: %s\nwant signature: %s", signed, vectorSignature)
	}
	if !strings.Contains(signed, "SignedHeaders=host;x-amz-date") {
		t.Errorf("signed headers are not the vector's: %s", signed)
	}
	if !strings.Contains(signed, "Credential="+vectorAccessKeyID+"/20150830/us-east-1/service/aws4_request") {
		t.Errorf("credential scope is not the vector's: %s", signed)
	}
}

// TestAPayloadHashIsRequired proves the refusal is real: an unsigned payload is
// a body an intermediary may replace, and the empty string is what a caller
// that forgot would pass.
func TestAPayloadHashIsRequired(t *testing.T) {
	request, err := http.NewRequest(http.MethodPut, "https://bucket.s3.us-west-2.amazonaws.com/key", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if err := Sign(request, "", Credentials{AccessKeyID: "a", SecretAccessKey: "b"}, "us-west-2", "s3", time.Now()); err == nil {
		t.Fatal("signing accepted an empty payload hash; an unsigned body can be replaced in flight")
	}
}

// TestASessionTokenIsSignedNotMerelySent is the ECS case. A task role issues
// temporary credentials, and S3 refuses a signature whose signed-header list
// omits a security token that is present on the request — which reads as an
// authentication failure with no mention of the token.
func TestASessionTokenIsSignedNotMerelySent(t *testing.T) {
	request, err := http.NewRequest(http.MethodPut, "https://bucket.s3.us-west-2.amazonaws.com/key", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	err = Sign(request, HashPayload([]byte("{}")), Credentials{
		AccessKeyID:     "id",
		SecretAccessKey: "secret",
		SessionToken:    "token",
	}, "us-west-2", "s3", time.Now())
	if err != nil {
		t.Fatalf("signing: %v", err)
	}

	if request.Header.Get("X-Amz-Security-Token") != "token" {
		t.Error("the session token was not sent")
	}
	if !strings.Contains(request.Header.Get("Authorization"), "x-amz-security-token") {
		t.Errorf("the session token was sent but not signed: %s", request.Header.Get("Authorization"))
	}
}

// TestTheContentHashIsSignedForS3 is the converse of the vector test: the
// header the vector predates is the one S3 requires, so its absence from the
// signed set would be a working signature that S3 rejects.
func TestTheContentHashIsSignedForS3(t *testing.T) {
	request, err := http.NewRequest(http.MethodPut, "https://bucket.s3.us-west-2.amazonaws.com/inventory/current.json", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	body := []byte(`{"snapshotId":"x"}`)
	if err := Sign(request, HashPayload(body), Credentials{AccessKeyID: "id", SecretAccessKey: "secret"}, "us-west-2", "s3", time.Now()); err != nil {
		t.Fatalf("signing: %v", err)
	}

	if request.Header.Get("X-Amz-Content-Sha256") != HashPayload(body) {
		t.Error("the content hash header does not carry the body's hash")
	}
	if !strings.Contains(request.Header.Get("Authorization"), "x-amz-content-sha256") {
		t.Errorf("the content hash was not signed: %s", request.Header.Get("Authorization"))
	}
}

// TestTheSignatureCoversTheBody is the property the whole package exists for:
// two different bodies must not produce one signature.
func TestTheSignatureCoversTheBody(t *testing.T) {
	sign := func(body []byte) string {
		request, err := http.NewRequest(http.MethodPut, "https://bucket.s3.us-west-2.amazonaws.com/key", nil)
		if err != nil {
			t.Fatalf("building the request: %v", err)
		}
		at := vectorTime(t)
		if err := Sign(request, HashPayload(body), Credentials{AccessKeyID: "id", SecretAccessKey: "secret"}, "us-west-2", "s3", at); err != nil {
			t.Fatalf("signing: %v", err)
		}
		return request.Header.Get("Authorization")
	}

	if sign([]byte(`{"a":1}`)) == sign([]byte(`{"a":2}`)) {
		t.Fatal("two different bodies produced the same signature")
	}
}

// TestQueryStringIsCanonicalised pins the encoding, which `net/url` gets
// deliberately differently: a space is `%20` here and `+` there, and the
// difference is a signature mismatch with no diagnostic.
func TestQueryStringIsCanonicalised(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://bucket.s3.us-west-2.amazonaws.com/?b=two+parts&a=1", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if got, want := canonicalQuery(request), "a=1&b=two%20parts"; got != want {
		t.Errorf("canonical query = %q, want %q", got, want)
	}
}
