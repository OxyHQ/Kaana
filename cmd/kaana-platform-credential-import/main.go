// Command kaana-platform-credential-import sends one signed platform provider
// credential mutation. Provider plaintext is accepted only on stdin.
package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/platformcredentialcontrol"
	"github.com/OxyHQ/Kaana/internal/provider"
)

const maxSecretBytes = 4096

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	set := flag.NewFlagSet("kaana-platform-credential-import", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	var endpoint, operationID, providerSlug, keyID, class, actor, signingKeyID, signingKeyFile string
	var position int
	set.StringVar(&endpoint, "endpoint", "", "internal HTTPS mutation URL")
	set.StringVar(&operationID, "operation-id", "", "kpc_ operation id")
	set.StringVar(&providerSlug, "provider", "", "provider slug")
	set.StringVar(&keyID, "key-id", "", "opaque UUIDv4 key id")
	set.StringVar(&class, "class", "", "free or paid")
	set.IntVar(&position, "position", 0, "provider pool position")
	set.StringVar(&actor, "actor", "", "audited operator identity")
	set.StringVar(&signingKeyID, "signing-key-id", "", "public signing key id")
	set.StringVar(&signingKeyFile, "signing-key-file", "", "0600 file containing a base64 Ed25519 seed or private key")
	if err := set.Parse(args); err != nil || set.NArg() != 0 {
		return errors.New("usage: kaana-platform-credential-import --endpoint <https-url> --operation-id <kpc_id> --provider <slug> --key-id <uuid> --class <free|paid> --position <n> --actor <identity> --signing-key-id <id> --signing-key-file <path> < secret")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != platformcredentialcontrol.MutationPath {
		return errors.New("platform credential import: endpoint must be the exact HTTPS mutation URL without credentials, query or fragment")
	}
	if signingKeyID == "" || signingKeyFile == "" {
		return errors.New("platform credential import: signing key id and file are required")
	}
	privateKey, err := readPrivateKey(signingKeyFile)
	if err != nil {
		return err
	}
	defer clear(privateKey)
	secret, err := io.ReadAll(io.LimitReader(stdin, maxSecretBytes+1))
	if err != nil {
		return errors.New("platform credential import: could not read secret from stdin")
	}
	defer clear(secret)
	if len(secret) > 0 && secret[len(secret)-1] == '\n' {
		secret = secret[:len(secret)-1]
	}
	mutation := credentialstore.PlatformCredentialMutation{
		SchemaVersion: 1, OperationID: operationID, Provider: contract.ProviderSlug(providerSlug),
		KeyID: keyID, SecretBase64: base64.StdEncoding.EncodeToString(secret), Class: provider.KeyClass(class),
		Position: position, OperationActor: actor,
	}
	body, err := json.Marshal(mutation)
	mutation.SecretBase64 = ""
	if err != nil {
		return errors.New("platform credential import: could not encode mutation")
	}
	defer clear(body)
	timestamp := time.Now().UnixMilli()
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("platform credential import: could not build request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(edgeauth.HeaderKeyID, signingKeyID)
	request.Header.Set(edgeauth.HeaderTimestamp, strconv.FormatInt(timestamp, 10))
	request.Header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, edgeauth.PlatformCredentialControlSigningInput(signingKeyID, timestamp, body))))
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		return errors.New("platform credential import: HTTPS request failed")
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	if err != nil {
		return errors.New("platform credential import: could not read response")
	}
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("platform credential import: service returned HTTP %d", response.StatusCode)
	}
	var receipt credentialstore.PlatformCredentialReceipt
	if json.Unmarshal(responseBody, &receipt) != nil || receipt.OperationID != operationID || string(receipt.Provider) != providerSlug || receipt.KeyID != keyID {
		return errors.New("platform credential import: service returned a mismatched receipt")
	}
	return json.NewEncoder(stdout).Encode(receipt)
}

func readPrivateKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("platform credential import: signing key file must be a regular file with mode 0600")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("platform credential import: could not read signing key file")
	}
	defer clear(encoded)
	encoded = bytes.TrimSpace(encoded)
	raw := make([]byte, base64.StdEncoding.DecodedLen(len(encoded)))
	written, err := base64.StdEncoding.Strict().Decode(raw, encoded)
	if err != nil {
		clear(raw)
		return nil, errors.New("platform credential import: signing key file is not strict base64")
	}
	raw = raw[:written]
	switch len(raw) {
	case ed25519.SeedSize:
		privateKey := ed25519.NewKeyFromSeed(raw)
		clear(raw)
		return privateKey, nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	default:
		clear(raw)
		return nil, errors.New("platform credential import: signing key has the wrong length")
	}
}
