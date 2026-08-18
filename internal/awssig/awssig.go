// Package awssig signs an HTTP request with AWS Signature Version 4.
//
// It exists because this module has no dependencies and the publisher needs to
// write exactly one object to S3. Pulling the AWS SDK in for a `GET` and a
// `PUT` would be this repository's first dependency, and it would arrive with a
// credential-resolution chain, a retry policy and a middleware stack that
// nothing here wants opinions from. The signing algorithm is public, fixed, and
// about a hundred lines; `awssig_test.go` checks it against AWS's own published
// test vector rather than against a second reading of the specification by the
// same author.
//
// It signs. It does not resolve credentials, choose a region, retry, or know
// what S3 is — those belong to the caller, so that the one thing this package
// can get catastrophically wrong is the one thing the vector pins.
package awssig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	algorithm = "AWS4-HMAC-SHA256"
	// The two spellings of an instant SigV4 uses. Both are derived from ONE
	// time value per signature: deriving the date from a second call to the
	// clock puts the scope in a different day from the timestamp for one
	// request a day, which fails as an unexplainable signature mismatch.
	timestampLayout = "20060102T150405Z"
	dateLayout      = "20060102"
)

// Credentials are what a signature is made with.
//
// SessionToken is empty for a long-lived key and set for the temporary
// credentials an ECS task role issues. When set it MUST be signed as
// `x-amz-security-token`, not merely sent: S3 rejects a signature whose signed
// headers omit a token that is present.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Sign adds the SigV4 headers to req in place.
//
// payloadHash is the hex sha256 of the body the caller will actually send. It
// is a parameter rather than something computed from req.Body because the
// signature has to cover the EXACT bytes that go on the wire; a signer that
// read and re-buffered the body would be free to sign something else, which is
// the classic way a signature check becomes decorative.
func Sign(req *http.Request, payloadHash string, creds Credentials, region, service string, at time.Time) error {
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return fmt.Errorf("awssig: incomplete credentials: a signature needs both an access key id and a secret")
	}
	if payloadHash == "" {
		return fmt.Errorf("awssig: no payload hash: an unsigned payload would let the body be replaced in flight")
	}

	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	return sign(req, payloadHash, creds, region, service, at)
}

// sign signs whatever headers are already on req, plus host and the date.
//
// Sign is the only caller in production; the split exists because AWS's
// published vector predates `x-amz-content-sha256` and signs exactly
// `host;x-amz-date`, so a known-answer test has to be able to reach the
// algorithm without the S3-specific header. The alternative — a parameter on
// Sign that suppresses it — would put "do not sign the payload" one wrong
// argument away in production code, to save a test four lines.
func sign(req *http.Request, payloadHash string, creds Credentials, region, service string, at time.Time) error {
	at = at.UTC()
	timestamp := at.Format(timestampLayout)
	date := at.Format(dateLayout)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	req.Header.Set("X-Amz-Date", timestamp)

	canonicalHeaders, signedHeaders := canonicalizeHeaders(req, host)

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req),
		canonicalQuery(req),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{date, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		algorithm,
		timestamp,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(signingKey(creds.SecretAccessKey, date, region, service), []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, creds.AccessKeyID, scope, signedHeaders, signature,
	))
	return nil
}

// HashPayload is the hex sha256 the caller passes to Sign and S3 requires in
// `x-amz-content-sha256`.
func HashPayload(body []byte) string { return hexSHA256(body) }

// canonicalizeHeaders renders the headers to sign, and the list naming them.
//
// Every header present is signed, not a chosen subset: the alternative is a
// list of "headers that matter", which is wrong the first time a caller adds
// one that does.
func canonicalizeHeaders(req *http.Request, host string) (string, string) {
	names := make([]string, 0, len(req.Header)+1)
	values := make(map[string]string, len(req.Header)+1)
	for name, headerValues := range req.Header {
		lower := strings.ToLower(name)
		trimmed := make([]string, 0, len(headerValues))
		for _, value := range headerValues {
			trimmed = append(trimmed, strings.TrimSpace(value))
		}
		names = append(names, lower)
		values[lower] = strings.Join(trimmed, ",")
	}
	if _, present := values["host"]; !present {
		// Go moves Host out of the header map onto the request, so it is
		// absent here and would otherwise go unsigned — which S3 refuses.
		names = append(names, "host")
		values["host"] = host
	}
	sort.Strings(names)

	var canonical strings.Builder
	for _, name := range names {
		canonical.WriteString(name)
		canonical.WriteString(":")
		canonical.WriteString(values[name])
		canonical.WriteString("\n")
	}
	return canonical.String(), strings.Join(names, ";")
}

// canonicalURI is the request path as S3 wants it signed.
//
// `EscapedPath` preserves the encoding the caller chose, which is what S3
// expects: unlike most services it does NOT re-encode the path a second time,
// so normalising here would sign a different key from the one being written.
func canonicalURI(req *http.Request) string {
	path := req.URL.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

// canonicalQuery renders the query string sorted, as the algorithm requires.
func canonicalQuery(req *http.Request) string {
	query := req.URL.Query()
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		sorted := append([]string(nil), query[key]...)
		sort.Strings(sorted)
		for _, value := range sorted {
			pairs = append(pairs, uriEncode(key)+"="+uriEncode(value))
		}
	}
	return strings.Join(pairs, "&")
}

// uriEncode is RFC 3986 encoding with the unreserved set AWS names.
//
// `net/url` is not usable here: it encodes a space as `+` in a query and leaves
// several sub-delimiters alone, and both differences change the signature.
func uriEncode(value string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var encoded strings.Builder
	for i := 0; i < len(value); i++ {
		character := value[i]
		if strings.IndexByte(unreserved, character) >= 0 {
			encoded.WriteByte(character)
			continue
		}
		fmt.Fprintf(&encoded, "%%%02X", character)
	}
	return encoded.String()
}

// signingKey derives the request-scoped key. Scoping it to a date, a region and
// a service is what stops a captured signature being replayed against another.
func signingKey(secret, date, region, service string) []byte {
	key := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	key = hmacSHA256(key, []byte(region))
	key = hmacSHA256(key, []byte(service))
	return hmacSHA256(key, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
