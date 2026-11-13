// Command kaana-native performs explicit, reviewed schema/enrollment operations.
// Serving workers have no authority to execute these administration transactions.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/nativeadmission"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string, stdin io.Reader, stdout io.Writer, getenv func(string) string) error {
	if len(args) > 0 && args[0] == "identity-check" {
		return runIdentityCheck(args[1:], stdout, getenv, openIdentityCheck)
	}
	if len(args) > 0 && args[0] == "inspect-access" {
		return runInspectAccess(args[1:], stdout, getenv)
	}
	if len(args) == 0 {
		return errors.New("usage: kaana-native migrate|register|import|validate")
	}
	flags := flag.NewFlagSet("kaana-native", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("policy-file", "", "reviewed policy file")
	fileHash := flags.String("policy-file-sha256", "", "reviewed file SHA256")
	policyID := flags.String("policy-id", "", "reviewed policy ID")
	policyVersion := flags.Int("policy-version", 0, "reviewed version")
	policyHash := flags.String("policy-sha256", "", "reviewed immutable policy digest")
	manifestHash := flags.String("manifest-sha256", "", "reviewed exported manifest hash")
	fenceID := flags.String("fence-id", "", "reviewed Oxy fence epoch")
	activated := flags.String("activated-at", "", "exact reviewed activation timestamp")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return errors.New("native command arguments invalid")
	}
	if args[0] != "migrate" && args[0] != "register" && args[0] != "import" && args[0] != "validate" {
		return errors.New("unknown native command")
	}
	var selected *nativeadmission.Policy
	var policies []nativeadmission.Policy
	if args[0] != "migrate" {
		var err error
		policies, err = nativeadmission.LoadPolicyFile(*path, *fileHash)
		if err != nil {
			return err
		}
		for _, p := range policies {
			if p.ID == *policyID && p.Version == *policyVersion {
				copy := p
				selected = &copy
			}
		}
		if selected == nil {
			return errors.New("reviewed policy ID/version is absent")
		}
		hash, err := nativeadmission.PolicyDigest(*selected)
		if err != nil || hash != *policyHash {
			return errors.New("reviewed policy digest mismatch")
		}
	}
	var manifest []byte
	review := nativeadmission.ImportReview{}
	if args[0] == "import" || args[0] == "validate" {
		var err error
		manifest, err = io.ReadAll(io.LimitReader(stdin, 64*1024*1024+1))
		if err != nil || len(manifest) > 64*1024*1024 {
			return errors.New("manifest exceeds bound or cannot be read")
		}
		review = nativeadmission.ImportReview{Policy: *selected, PolicyDigest: *policyHash, ManifestSHA256: *manifestHash, FenceID: *fenceID, ActivatedAt: *activated}
		parsed, err := nativeadmission.DecodeLegacyManifest(manifest, review, time.Now())
		if err != nil {
			return err
		}
		if args[0] == "validate" {
			return json.NewEncoder(stdout).Encode(map[string]any{"valid": true, "manifestSha256": parsed.SHA256, "entries": len(parsed.Entries), "eligibleCredentials": len(parsed.Population.CredentialIDs), "policyDigest": *policyHash})
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	url := strings.TrimSpace(getenv("DATABASE_URL"))
	if url == "" {
		return errors.New("database configuration is required")
	}
	store, closePool, err := nativeadmission.OpenAdministration(ctx, url, args[0] == "migrate")
	if err != nil {
		return err
	}
	defer closePool()

	switch args[0] {
	case "migrate":
		err = store.Migrate(ctx)
	case "register":
		err = store.RegisterPolicy(ctx, *selected)
	case "import":
		var result nativeadmission.ImportResult
		result, err = store.ImportLegacyFence(ctx, manifest, review)
		if err == nil {
			return json.NewEncoder(stdout).Encode(result)
		}
	}
	if err != nil {
		return errors.New("native administration transaction refused; no partial activation committed")
	}
	return json.NewEncoder(stdout).Encode(map[string]string{"status": "complete", "operation": args[0]})
}
