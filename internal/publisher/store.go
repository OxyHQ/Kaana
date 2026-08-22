package publisher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/awssig"
	"github.com/OxyHQ/Kaana/internal/contract"
)

// ObjectStore is where the snapshot is published.
//
// An interface because the publish loop's behaviour under a store that is
// missing, unreadable or failing is most of what there is to test, and a test
// that reached a real bucket would be testing AWS.
type ObjectStore interface {
	// Get reads the currently published snapshot. The bool is false when
	// nothing has been published yet, which is a first run and NOT an error —
	// the distinction is the whole of the re-dating safety property.
	Get(ctx context.Context) ([]byte, bool, error)
	// Put replaces the published snapshot. It is a whole-object write: S3 has
	// no partial PUT, so a reader never sees a half-written object from here.
	Put(ctx context.Context, body []byte) error
	// Describe names the destination for a log line, without a credential.
	Describe() string
}

// S3Store publishes to one S3 object.
type S3Store struct {
	client      *http.Client
	bucket      string
	key         string
	region      string
	credentials CredentialSource
	// now is injectable so a signature's instant is testable.
	now func() time.Time
}

// NewS3Store wires a store, refusing a destination it could only guess at.
func NewS3Store(client *http.Client, bucket, key, region string, credentials CredentialSource) (*S3Store, error) {
	switch {
	case bucket == "":
		// Never defaulted. A plausible default bucket turns a missing variable
		// into "published somewhere else, everything green", which is the exact
		// class of silent failure this whole artefact is designed against.
		return nil, fmt.Errorf("publisher: no bucket: set KAANA_INVENTORY_BUCKET; there is no default, because publishing to a guessed bucket succeeds silently")
	case key == "":
		return nil, fmt.Errorf("publisher: no object key: set KAANA_INVENTORY_KEY")
	case region == "":
		return nil, fmt.Errorf("publisher: no region: set AWS_REGION")
	case credentials == nil:
		return nil, fmt.Errorf("publisher: no credential source")
	}
	return &S3Store{
		client:      client,
		bucket:      bucket,
		key:         strings.TrimPrefix(key, "/"),
		region:      region,
		credentials: credentials,
		now:         time.Now,
	}, nil
}

// Describe names bucket and key. Neither is a secret; the credential is, and is
// not reachable from here.
func (s *S3Store) Describe() string { return "s3://" + s.bucket + "/" + s.key }

func (s *S3Store) url() string {
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", s.bucket, s.region, s.key)
}

// Get reads the published snapshot, distinguishing absent from unreadable.
func (s *S3Store) Get(ctx context.Context) ([]byte, bool, error) {
	response, err := s.send(ctx, http.MethodGet, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxSnapshotBytes))
	if err != nil {
		return nil, false, fmt.Errorf("publisher: reading %s: %w", s.Describe(), err)
	}
	switch response.StatusCode {
	case http.StatusOK:
		return body, true, nil
	case http.StatusNotFound:
		// Nothing published yet. The ONLY status that means "first run".
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("publisher: reading %s answered %d: %s",
			s.Describe(), response.StatusCode, contract.SafeErrorText(string(body)))
	}
}

// Put writes the snapshot.
func (s *S3Store) Put(ctx context.Context, body []byte) error {
	response, err := s.send(ctx, http.MethodPut, body)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	answer, err := io.ReadAll(io.LimitReader(response.Body, maxSnapshotBytes))
	if err != nil {
		return fmt.Errorf("publisher: writing %s: %w", s.Describe(), err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("publisher: writing %s answered %d: %s",
			s.Describe(), response.StatusCode, contract.SafeErrorText(string(answer)))
	}
	return nil
}

func (s *S3Store) send(ctx context.Context, method string, body []byte) (*http.Response, error) {
	credentials, err := s.credentials(ctx)
	if err != nil {
		return nil, err
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, s.url(), reader)
	if err != nil {
		return nil, fmt.Errorf("publisher: building the %s request for %s: %w", method, s.Describe(), err)
	}
	if body != nil {
		request.ContentLength = int64(len(body))
		request.Header.Set("Content-Type", "application/json")
	}

	// The hash covers the exact bytes handed to the reader above. Re-encoding
	// between signing and sending would authenticate something other than what
	// is written.
	if err := awssig.Sign(request, awssig.HashPayload(body), credentials, s.region, "s3", s.now()); err != nil {
		return nil, err
	}

	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("publisher: %s %s: %w", method, s.Describe(), err)
	}
	return response, nil
}

const maxSnapshotBytes = 4 << 20

// CredentialSource resolves the credentials a signature is made with.
type CredentialSource func(ctx context.Context) (awssig.Credentials, error)

// StaticCredentials is the local and CI lane: whatever the environment holds.
func StaticCredentials(accessKeyID, secretAccessKey, sessionToken string) CredentialSource {
	return func(context.Context) (awssig.Credentials, error) {
		if accessKeyID == "" || secretAccessKey == "" {
			return awssig.Credentials{}, fmt.Errorf("publisher: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are not both set, and no container credential endpoint was offered")
		}
		return awssig.Credentials{
			AccessKeyID:     accessKeyID,
			SecretAccessKey: secretAccessKey,
			SessionToken:    sessionToken,
		}, nil
	}
}

// ContainerCredentials is the ECS task-role lane.
//
// The agent hands out temporary credentials that EXPIRE, so they are refreshed
// rather than read once: a process that cached them at boot would sign
// perfectly until the first expiry and then fail every publish with a 403 that
// says nothing about time.
func ContainerCredentials(client *http.Client, endpoint string) CredentialSource {
	var (
		mu      sync.Mutex
		cached  awssig.Credentials
		expires time.Time
	)
	return func(ctx context.Context) (awssig.Credentials, error) {
		mu.Lock()
		defer mu.Unlock()

		// Refreshed a few minutes early: a credential that expires mid-flight
		// fails the request it was fetched for.
		if cached.AccessKeyID != "" && time.Now().Add(5*time.Minute).Before(expires) {
			return cached, nil
		}

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return awssig.Credentials{}, fmt.Errorf("publisher: building the container credential request: %w", err)
		}
		response, err := client.Do(request)
		if err != nil {
			return awssig.Credentials{}, fmt.Errorf("publisher: reading the task role's credentials: %w", err)
		}
		defer func() { _ = response.Body.Close() }()

		body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			return awssig.Credentials{}, fmt.Errorf("publisher: reading the task role's credentials: %w", err)
		}
		if response.StatusCode != http.StatusOK {
			return awssig.Credentials{}, fmt.Errorf("publisher: the container credential endpoint answered %d", response.StatusCode)
		}

		var payload struct {
			AccessKeyID     string `json:"AccessKeyId"`
			SecretAccessKey string `json:"SecretAccessKey"`
			Token           string `json:"Token"`
			Expiration      string `json:"Expiration"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return awssig.Credentials{}, fmt.Errorf("publisher: the container credential response is not readable: %w", err)
		}
		if payload.AccessKeyID == "" || payload.SecretAccessKey == "" {
			return awssig.Credentials{}, fmt.Errorf("publisher: the container credential response carries no key")
		}

		cached = awssig.Credentials{
			AccessKeyID:     payload.AccessKeyID,
			SecretAccessKey: payload.SecretAccessKey,
			SessionToken:    payload.Token,
		}
		if payload.Expiration != "" {
			parsed, err := time.Parse(time.RFC3339, payload.Expiration)
			if err != nil {
				return awssig.Credentials{}, fmt.Errorf("publisher: the container credential expiry %q is not an instant: %w", payload.Expiration, err)
			}
			expires = parsed
		} else {
			// No expiry offered: treat it as short-lived rather than eternal,
			// so the failure is an extra fetch and not a signature that stops
			// working with no explanation.
			expires = time.Now().Add(10 * time.Minute)
		}
		return cached, nil
	}
}
