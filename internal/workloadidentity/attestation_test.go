package workloadidentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/OxyHQ/Kaana/internal/awssig"
)

const (
	testAccessKeyID     = "ASIAEXAMPLETASKROLE"
	testSecretAccessKey = "wJalrXUtnFEMI-K7MDENG-bPxRfiCYEXAMPLEKEY"
	testSessionToken    = "FQoGZXIvYXdzEExampleSessionTokenForATaskRole=="
)

func taskRoleCredentials() aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{
			AccessKeyID:     testAccessKeyID,
			SecretAccessKey: testSecretAccessKey,
			SessionToken:    testSessionToken,
			Source:          "test",
		}, nil
	})
}

// oxy is a fake Oxy that applies exactly the checks the real verifier applies
// (packages/api/src/services/workloadAttestation.service.ts) and then replays
// the attestation to a fake STS that RE-DERIVES the signature.
//
// The re-derivation is the point. A fake that only looked at the payload's shape
// would pass an attestation whose signature covered different headers from the
// ones sent, which is the one mistake that cannot be caught anywhere but on the
// wire; `TestAttestationIsRefusedWhenItIsTamperedWith` is the control proving
// this fake refuses when that happens.
type oxy struct {
	server *httptest.Server

	mu          sync.Mutex
	nonce       string
	challenges  int
	exchanges   int
	rawExchange string
	hosts       []string

	// Test hooks.
	challengeStatus int
	challengeBody   string
	exchangeBody    string
	tamper          func(map[string]string)
	credentials     awssig.Credentials
	now             time.Time
}

func newOxy(t *testing.T) *oxy {
	t.Helper()
	fake := &oxy{
		nonce: "Yy1ub25jZS1mcm9tLW9yaWdpbi0wMDAwMDAwMDAwMDAwMDAw",
		credentials: awssig.Credentials{
			AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey, SessionToken: testSessionToken,
		},
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.server.Close)
	return fake
}

func (o *oxy) origin(t *testing.T) *url.URL {
	t.Helper()
	parsed, err := url.Parse(o.server.URL)
	if err != nil {
		t.Fatalf("parsing the fake origin: %v", err)
	}
	return parsed
}

func (o *oxy) handle(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.hosts = append(o.hosts, r.Host)
	switch r.URL.Path {
	case "/" + challengePath:
		o.challenges++
		if o.challengeStatus != 0 && o.challengeStatus != http.StatusOK {
			http.Error(w, "no", o.challengeStatus)
			return
		}
		body := o.challengeBody
		if body == "" {
			body = fmt.Sprintf(`{"data":{"nonce":%q,"expiresIn":60}}`, o.nonce)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	case "/" + exchangePath:
		o.exchanges++
		raw, _ := io.ReadAll(r.Body)
		o.rawExchange = string(raw)
		var presented attestationRequest
		if err := json.Unmarshal(raw, &presented); err != nil {
			http.Error(w, "malformed", http.StatusBadRequest)
			return
		}
		if o.tamper != nil {
			o.tamper(presented.Attestation.Headers)
		}
		if reason := o.verify(presented); reason != "" {
			http.Error(w, reason, http.StatusUnauthorized)
			return
		}
		body := o.exchangeBody
		if body == "" {
			body = `{"data":{"token":"oxy-service-token-from-attestation","expiresIn":3600,"appName":"Kaana"}}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	default:
		http.NotFound(w, r)
	}
}

// verify mirrors Oxy's refusals, in Oxy's order, and returns its reason string.
func (o *oxy) verify(presented attestationRequest) string {
	if presented.Provider != attestationProvider {
		return "unsupported_provider"
	}
	if presented.Nonce != o.nonce {
		return "unknown_challenge"
	}
	headers := presented.Attestation.Headers
	if headers["host"] != stsHost {
		return "host_not_sts"
	}
	authorization := headers["authorization"]
	if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 ") {
		return "unsigned"
	}
	signed := signedHeaderNames(authorization)
	if !contains(signed, nonceHeader) {
		return "nonce_unsigned"
	}
	if headers[nonceHeader] != o.nonce {
		return "nonce_mismatch"
	}
	signedAt, err := time.Parse("20060102T150405Z", headers["x-amz-date"])
	if err != nil {
		return "stale"
	}
	reference := o.now
	if reference.IsZero() {
		reference = time.Now()
	}
	if signedAt.Sub(reference).Abs() > 5*time.Minute {
		return "stale"
	}
	if err := stsWouldAccept(headers, signed, o.credentials); err != nil {
		return "sts_rejected"
	}
	return ""
}

// stsWouldAccept re-derives the signature from the headers as RECEIVED, the way
// STS does: only the headers SignedHeaders names, the request Oxy replays, and
// the instant the signature claims.
func stsWouldAccept(headers map[string]string, signed []string, credentials awssig.Credentials) error {
	replay, err := http.NewRequest(http.MethodPost, "https://"+headers["host"]+"/", nil)
	if err != nil {
		return err
	}
	for _, name := range signed {
		if name == "host" {
			continue
		}
		value, present := headers[name]
		if !present {
			return fmt.Errorf("signed header %q was not sent", name)
		}
		replay.Header.Set(name, value)
	}
	signedAt, err := time.Parse("20060102T150405Z", headers["x-amz-date"])
	if err != nil {
		return err
	}
	if err := awssig.Sign(replay, awssig.HashPayload([]byte(stsBody)), credentials, stsRegion, stsService, signedAt); err != nil {
		return err
	}
	if replay.Header.Get("Authorization") != headers["authorization"] {
		return errors.New("the signature does not cover the headers that were sent")
	}
	return nil
}

func signedHeaderNames(authorization string) []string {
	for _, part := range strings.Split(authorization, ", ") {
		if rest, found := strings.CutPrefix(part, "SignedHeaders="); found {
			return strings.Split(rest, ";")
		}
	}
	return nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func newTestClient(t *testing.T, credentials aws.CredentialsProvider) *Client {
	t.Helper()
	client, err := New(Config{
		Credentials: credentials,
		Logger:      slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func TestMintExchangesAnAttestationOxyAndSTSAccept(t *testing.T) {
	fake := newOxy(t)
	client := newTestClient(t, taskRoleCredentials())

	token, lifetime, err := client.Mint(context.Background(), fake.origin(t))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if token != "oxy-service-token-from-attestation" {
		t.Fatalf("token = %q", token)
	}
	if lifetime != time.Hour {
		t.Fatalf("lifetime = %s, want 1h as Oxy reported it", lifetime)
	}
	if fake.challenges != 1 || fake.exchanges != 1 {
		t.Fatalf("challenges = %d, exchanges = %d; want one of each", fake.challenges, fake.exchanges)
	}
}

// The attestation must carry the signature and nothing that is not part of it.
func TestAttestationCarriesExactlyTheSignedHeaders(t *testing.T) {
	fake := newOxy(t)
	client := newTestClient(t, taskRoleCredentials())

	if _, _, err := client.Mint(context.Background(), fake.origin(t)); err != nil {
		t.Fatalf("Mint: %v", err)
	}

	var presented map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fake.rawExchange), &presented); err != nil {
		t.Fatalf("decoding the presented body: %v", err)
	}
	keys := sortedKeys(presented)
	if strings.Join(keys, ",") != "attestation,nonce,provider" {
		t.Fatalf("exchange body keys = %v; Oxy accepts exactly provider, nonce and attestation", keys)
	}

	var envelope attestationRequest
	if err := json.Unmarshal([]byte(fake.rawExchange), &envelope); err != nil {
		t.Fatalf("decoding the attestation: %v", err)
	}
	if envelope.Provider != "aws-iam" {
		t.Fatalf("provider = %q", envelope.Provider)
	}
	got := sortedKeys(envelope.Attestation.Headers)
	want := []string{"authorization", "host", "x-amz-content-sha256", "x-amz-date", "x-amz-security-token", nonceHeader}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("attestation headers = %v, want %v", got, want)
	}
	if envelope.Attestation.Headers["host"] != stsHost {
		t.Fatalf("host = %q", envelope.Attestation.Headers["host"])
	}
	// Every one of them is signed. A header Oxy replays but the signature does
	// not cover is a header an attacker can change.
	signed := signedHeaderNames(envelope.Attestation.Headers["authorization"])
	for _, name := range got {
		if name == "authorization" {
			continue
		}
		if !contains(signed, name) {
			t.Errorf("header %q travels unsigned", name)
		}
	}
}

// A task with no session token — which is not how ECS hands out credentials, but
// is how a long-lived key looks — must not sign a security token it does not have.
func TestAttestationOmitsTheSecurityTokenWhenThereIsNone(t *testing.T) {
	fake := newOxy(t)
	fake.credentials = awssig.Credentials{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey}
	client := newTestClient(t, aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey}, nil
	}))

	if _, _, err := client.Mint(context.Background(), fake.origin(t)); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	var envelope attestationRequest
	if err := json.Unmarshal([]byte(fake.rawExchange), &envelope); err != nil {
		t.Fatalf("decoding the attestation: %v", err)
	}
	if _, present := envelope.Attestation.Headers["x-amz-security-token"]; present {
		t.Fatal("an attestation signed without a session token must not carry one")
	}
}

// The positive control for the fake: when the attestation is altered between
// signing and presenting, the replay has to fail. Without this, every assertion
// above would also hold for a fake that checked nothing.
func TestAttestationIsRefusedWhenItIsTamperedWith(t *testing.T) {
	for name, tamper := range map[string]func(map[string]string){
		"the nonce is swapped": func(headers map[string]string) {
			headers[nonceHeader] = "some-other-nonce"
		},
		"the signature is edited": func(headers map[string]string) {
			headers["authorization"] = flipLastCharacter(headers["authorization"])
		},
		"the nonce is dropped from the signed set": func(headers map[string]string) {
			headers["authorization"] = strings.Replace(headers["authorization"], ";"+nonceHeader, "", 1)
		},
		"the signing instant is moved": func(headers map[string]string) {
			headers["x-amz-date"] = "20200102T030405Z"
		},
		"the host is redirected": func(headers map[string]string) {
			headers["host"] = "sts.amazonaws.com.attacker.example"
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newOxy(t)
			fake.tamper = tamper
			client := newTestClient(t, taskRoleCredentials())

			_, _, err := client.Mint(context.Background(), fake.origin(t))
			if err == nil {
				t.Fatal("a tampered attestation was accepted")
			}
			if !strings.Contains(err.Error(), "HTTP 401") {
				t.Fatalf("error = %v; want Oxy's refusal", err)
			}
		})
	}
}

// Nothing is sent to AWS. The signature is the artefact; calling STS ourselves
// would only tell this process what it already knows, and would put the task
// role's identity on a second wire for no reason.
func TestMintNeverCallsAWS(t *testing.T) {
	fake := newOxy(t)
	client := newTestClient(t, taskRoleCredentials())

	if _, _, err := client.Mint(context.Background(), fake.origin(t)); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	for _, host := range fake.hosts {
		if host != fake.origin(t).Host {
			t.Fatalf("contacted %q", host)
		}
	}
	if strings.Contains(fake.rawExchange, "amazonaws.com/") {
		t.Fatal("the attestation carries a URL rather than a signature")
	}
}

func TestMintRefusals(t *testing.T) {
	cases := map[string]struct {
		configure   func(*oxy)
		credentials aws.CredentialsProvider
		origin      func(*testing.T, *oxy) *url.URL
		wantError   string
	}{
		"oxy will not issue a challenge": {
			configure: func(o *oxy) { o.challengeStatus = http.StatusServiceUnavailable },
			wantError: "Oxy refused to issue a challenge: HTTP 503",
		},
		"the challenge has no nonce": {
			configure: func(o *oxy) { o.challengeBody = `{"data":{"expiresIn":60}}` },
			wantError: "issued a challenge with no nonce",
		},
		"the challenge is not json": {
			configure: func(o *oxy) { o.challengeBody = `<html>no</html>` },
			wantError: "challenge response is invalid",
		},
		"this task has no credentials": {
			credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{}, errors.New("the container credentials endpoint answered 500")
			}),
			wantError: "resolving this task's AWS credentials",
		},
		"oxy answers without a token": {
			configure: func(o *oxy) { o.exchangeBody = `{"data":{"expiresIn":3600,"appName":"Kaana"}}` },
			wantError: "without a usable token",
		},
		"oxy answers with a token that never expires": {
			configure: func(o *oxy) { o.exchangeBody = `{"data":{"token":"t","expiresIn":0}}` },
			wantError: "without a usable token",
		},
		"there is no origin to attest to": {
			origin:    func(*testing.T, *oxy) *url.URL { return nil },
			wantError: "an Oxy API origin is required",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newOxy(t)
			if testCase.configure != nil {
				testCase.configure(fake)
			}
			credentials := testCase.credentials
			if credentials == nil {
				credentials = taskRoleCredentials()
			}
			client := newTestClient(t, credentials)
			origin := fake.origin(t)
			if testCase.origin != nil {
				origin = testCase.origin(t, fake)
			}
			_, _, err := client.Mint(context.Background(), origin)
			if err == nil {
				t.Fatalf("Mint succeeded; want %q", testCase.wantError)
			}
			if !strings.Contains(err.Error(), testCase.wantError) {
				t.Fatalf("error = %v, want it to mention %q", err, testCase.wantError)
			}
			if strings.Contains(err.Error(), testSecretAccessKey) || strings.Contains(err.Error(), testSessionToken) {
				t.Fatal("the error carries AWS credential material")
			}
		})
	}
}

// Oxy's routes answer `{data:…}`; its own client accepts the unwrapped form too,
// so this one does.
func TestMintAcceptsAnUnwrappedResponse(t *testing.T) {
	fake := newOxy(t)
	fake.challengeBody = fmt.Sprintf(`{"nonce":%q,"expiresIn":60}`, fake.nonce)
	fake.exchangeBody = `{"token":"flat-token","expiresIn":900,"appName":"Kaana"}`
	client := newTestClient(t, taskRoleCredentials())

	token, lifetime, err := client.Mint(context.Background(), fake.origin(t))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if token != "flat-token" || lifetime != 15*time.Minute {
		t.Fatalf("token = %q, lifetime = %s", token, lifetime)
	}
}

// A refusal Oxy expresses as a 403 with a reason must not become a token, and
// must not put Oxy's body in the error.
func TestMintReportsOxysRefusalWithoutEchoingIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, challengePath) {
			_, _ = io.WriteString(w, `{"data":{"nonce":"n","expiresIn":60}}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"reason":"unbound_workload","subject":"arn:aws:iam::237343248947:role/oxy-kaana-task"}}`)
	}))
	defer server.Close()
	origin, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parsing the origin: %v", err)
	}
	client, err := New(Config{Credentials: taskRoleCredentials(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = client.Mint(context.Background(), origin)
	if err == nil {
		t.Fatal("an unbound workload was accepted")
	}
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "oxy-kaana-task") || strings.Contains(err.Error(), "unbound_workload") {
		t.Fatalf("the error echoed Oxy's body: %v", err)
	}
}

func TestMintCarriesTheCallersDeadline(t *testing.T) {
	fake := newOxy(t)
	client := newTestClient(t, taskRoleCredentials())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := client.Mint(ctx, fake.origin(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want a wrapped context.Canceled", err)
	}
}

func TestAttestableReportsOnlyAnECSTaskRole(t *testing.T) {
	cases := map[string]struct {
		environment map[string]string
		want        bool
	}{
		"an ECS task": {
			environment: map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/9c1f"},
			want:        true,
		},
		"an ECS task on the full endpoint": {
			environment: map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://169.254.170.2/v2/credentials/9c1f"},
			want:        true,
		},
		"a laptop with a personal IAM user": {
			environment: map[string]string{"AWS_ACCESS_KEY_ID": "AKIA", "AWS_SECRET_ACCESS_KEY": "secret", "AWS_PROFILE": "oxy"},
			want:        false,
		},
		"a CI runner": {
			environment: map[string]string{},
			want:        false,
		},
		"an empty variable is not an identity": {
			environment: map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "  "},
			want:        false,
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			lookup := func(key string) (string, bool) {
				value, present := testCase.environment[key]
				return value, present
			}
			if got := Attestable(lookup); got != testCase.want {
				t.Fatalf("Attestable = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestNewRefusesWithoutACredentialSource(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("a client with nothing to sign with was built")
	}
}

func TestOpenReportsNoWorkloadIdentityOffECS(t *testing.T) {
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "")
	if _, err := Open(context.Background(), Config{}); !errors.Is(err, ErrNoWorkloadIdentity) {
		t.Fatalf("Open error = %v, want ErrNoWorkloadIdentity", err)
	}
}

// flipLastCharacter makes a mutation that is guaranteed to apply: a mutation
// that silently did nothing is indistinguishable from one the test survived.
func flipLastCharacter(value string) string {
	if value == "" {
		return "0"
	}
	replacement := byte('0')
	if value[len(value)-1] == '0' {
		replacement = '1'
	}
	return value[:len(value)-1] + string(replacement)
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
