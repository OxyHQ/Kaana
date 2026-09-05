package branding

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode"
)

const (
	repositoryRoot     = "../.."
	canonicalLogoPath  = "docs/assets/kaana-logo.svg"
	canonicalLogoSHA   = "69fae83bc32a7df5a083616160ffc978a3256f30a918cb0e512567442b7a2be2"
	canonicalOrigin    = "https://kaana.ai"
	canonicalGoModule  = "module github.com/OxyHQ/Kaana"
	brandingTestSource = "internal/branding/branding_test.go"
)

var forbiddenIdentityMarkers = []string{
	"kaana.oxy.so",
	"relay.oxy.so",
	"github.com/oxyhq/relay",
	"@oxyhq/relay",
	"cmd/relay",
	"x-oxy-relay-",
	"relay_base_url",
	"relay_allowed_origins",
	"relay_edge_",
	"alia_relay_",
}

var legacyCredentialFiles = []string{
	".github/credential-admin-operations.json",
	"cmd/kaana-credentials/main_test.go",
	"cmd/kaana-credentials/workflow_test.go",
	"docs/operating.md",
	"docs/provider-credential-id-cutover.md",
	"internal/credentialstore/admin_postgres_integration_test.go",
	"internal/credentialstore/ssm.go",
	"internal/credentialstore/ssm_test.go",
	"internal/credentialstore/store_test.go",
}

func TestCanonicalBrandAssetAndRepositoryMetadata(t *testing.T) {
	logo := readRepositoryFile(t, canonicalLogoPath)
	if !isCanonicalLogo(logo) {
		t.Fatalf("%s SHA-256 = %x, want canonical Kaana source %s", canonicalLogoPath, sha256.Sum256(logo), canonicalLogoSHA)
	}

	readme := string(readRepositoryFile(t, "README.md"))
	for _, required := range []string{
		`<img src="` + canonicalLogoPath + `" alt="Kaana"`,
		"# Kaana",
		canonicalOrigin,
	} {
		if !strings.Contains(readme, required) {
			t.Errorf("README.md does not declare %q", required)
		}
	}

	module := string(readRepositoryFile(t, "go.mod"))
	if firstLine, _, _ := strings.Cut(module, "\n"); firstLine != canonicalGoModule {
		t.Errorf("go.mod first line = %q, want %q", firstLine, canonicalGoModule)
	}

	assetDocumentation := string(readRepositoryFile(t, "docs/assets/README.md"))
	if !strings.Contains(assetDocumentation, canonicalLogoSHA) {
		t.Errorf("docs/assets/README.md does not record the canonical logo checksum")
	}
}

func TestFormerInferenceIdentityIsQuarantined(t *testing.T) {
	legacyFiles := make(map[string]bool)
	problems := walkRepositoryIdentity(repositoryRoot, legacyFiles)
	if len(problems) != 0 {
		t.Fatalf("former inference identity escaped its migration-only boundary:\n%s", strings.Join(problems, "\n"))
	}

	found := make([]string, 0, len(legacyFiles))
	for path := range legacyFiles {
		found = append(found, path)
	}
	slices.Sort(found)
	if !slices.Equal(found, legacyCredentialFiles) {
		t.Fatalf("legacy provider-key references are in %v, want exact migration-only set %v", found, legacyCredentialFiles)
	}
}

func TestFormerInferenceIdentityGateDetectsRegressions(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		contents string
		want     string
	}{
		{
			name:     "retired Kaana hostname",
			path:     "README.md",
			contents: "Call https://kaana" + ".oxy.so for inference.",
			want:     "kaana" + ".oxy.so",
		},
		{
			name:     "former Relay hostname",
			path:     "config/runtime.yaml",
			contents: "origin: https://relay" + ".oxy.so",
			want:     "relay" + ".oxy.so",
		},
		{
			name:     "former Relay environment",
			path:     "cmd/kaana/main.go",
			contents: "RELAY" + "_BASE_URL",
			want:     "relay" + "_base_url",
		},
		{
			name:     "Alia provider key outside migration",
			path:     "docs/quick-start.md",
			contents: "/oxy/alia/" + "PROVIDER_KEY_OPENAI",
			want:     "legacy provider-key identifier",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			problems, _ := inspectIdentity(test.path, []byte(test.contents))
			if len(problems) == 0 {
				t.Fatalf("mutation was not detected")
			}
			if !strings.Contains(strings.Join(problems, "\n"), test.want) {
				t.Errorf("reported %v, want a problem containing %q", problems, test.want)
			}
		})
	}

	problems, legacy := inspectIdentity("internal/credentialstore/ssm.go", []byte("/oxy/relay/"+"RELAY_PROVIDER_XAI_API_KEY"))
	if len(problems) != 0 || !legacy {
		t.Fatalf("the explicit one-time migration boundary was not accepted: problems=%v legacy=%v", problems, legacy)
	}
}

func TestFormerInferenceIdentityScanSkipsInstalledDependencies(t *testing.T) {
	root := t.TempDir()
	dependency := filepath.Join(root, "tools", "contract", "node_modules", "@oxyhq", "contracts", "dist", "devicePairing.js")
	if err := os.MkdirAll(filepath.Dir(dependency), 0o700); err != nil {
		t.Fatalf("creating dependency tree: %v", err)
	}
	if err := os.WriteFile(dependency, []byte("A generic relay-abuse bound protects encrypted device pairing."), 0o600); err != nil {
		t.Fatalf("writing dependency fixture: %v", err)
	}

	problems := walkRepositoryIdentity(root, make(map[string]bool))
	if len(problems) != 0 {
		t.Fatalf("installed third-party code was treated as repository identity: %v", problems)
	}

	source := filepath.Join(root, "internal", "example.go")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatalf("creating source tree: %v", err)
	}
	if err := os.WriteFile(source, []byte("Call https://relay.oxy.so for inference."), 0o600); err != nil {
		t.Fatalf("writing source fixture: %v", err)
	}
	problems = walkRepositoryIdentity(root, make(map[string]bool))
	if len(problems) != 1 || !strings.Contains(problems[0], "internal/example.go") {
		t.Fatalf("repository source escaped the identity scan: %v", problems)
	}
}

func TestBrandAssetGateDetectsChangedBytes(t *testing.T) {
	logo := readRepositoryFile(t, canonicalLogoPath)
	mutated := slices.Clone(logo)
	mutated[len(mutated)/2] ^= 1
	if isCanonicalLogo(mutated) {
		t.Fatal("the canonical asset check accepted changed logo bytes")
	}
}

func isCanonicalLogo(contents []byte) bool {
	return fmt.Sprintf("%x", sha256.Sum256(contents)) == canonicalLogoSHA
}

func walkRepositoryIdentity(root string, legacyFiles map[string]bool) []string {
	var problems []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "node_modules" || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if relative == "build" || relative == "dist" {
				return filepath.SkipDir
			}
			return nil
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == brandingTestSource || !textSurface(relative) {
			return nil
		}

		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fileProblems, legacy := inspectIdentity(relative, contents)
		problems = append(problems, fileProblems...)
		if legacy {
			legacyFiles[relative] = true
		}
		return nil
	})
	if err != nil {
		return append(problems, fmt.Sprintf("walking repository: %v", err))
	}
	return problems
}

func inspectIdentity(path string, contents []byte) ([]string, bool) {
	text := strings.ToLower(string(contents))
	var problems []string
	for _, marker := range forbiddenIdentityMarkers {
		if strings.Contains(text, marker) {
			problems = append(problems, fmt.Sprintf("%s contains retired active identity %q", path, marker))
		}
	}

	legacy := legacyCredentialIdentity(text)
	if legacy && !slices.Contains(legacyCredentialFiles, path) {
		problems = append(problems, fmt.Sprintf("%s contains a legacy provider-key identifier outside the exact migration allow-list", path))
	}
	return problems, legacy
}

func legacyCredentialIdentity(text string) bool {
	text = strings.ToLower(text)
	if strings.Contains(text, "/oxy/alia/provider_key_") ||
		strings.Contains(text, "/oxy/relay/relay_provider_") {
		return true
	}
	for _, token := range strings.FieldsFunc(text, func(character rune) bool {
		return !unicode.IsLetter(character) && !unicode.IsDigit(character) && character != '-'
	}) {
		if strings.HasPrefix(token, "legacy-alia-") ||
			strings.HasPrefix(token, "relay-") ||
			strings.Contains(token, "-relay-") {
			return true
		}
	}
	return false
}

func textSurface(path string) bool {
	switch filepath.Ext(path) {
	case ".cjs", ".css", ".env", ".go", ".html", ".js", ".json", ".md", ".mjs", ".mod", ".py", ".sh", ".sql", ".tf", ".toml", ".ts", ".tsx", ".txt", ".xml", ".yaml", ".yml":
		return true
	default:
		return filepath.Base(path) == "Dockerfile"
	}
}

func readRepositoryFile(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(repositoryRoot, filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return contents
}
