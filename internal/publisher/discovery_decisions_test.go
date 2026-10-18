package publisher

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func decisionsFixture(t *testing.T, name string) []byte {
	t.Helper()
	f, err := os.Open("testdata/openrouter-decisions/" + name + ".json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()
	r, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	}()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Jev is an ordinary OpenRouter deployment: the authenticated decisions list
// and the public zero-retention list publish it under its dated canonical
// slug, and without a zero-retention endpoint it is not published at all.
func TestJevPublishesUnderItsDatedCanonicalSlug(t *testing.T) {
	expanded := decisionsFixture(t, "text-decisions")
	zdr := decisionsFixture(t, "zdr")
	attribution, err := LoadAttribution("../../configs/model-attribution.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, withZDR := range []bool{true, false} {
		t.Run(map[bool]string{true: "zero-retention", false: "no-zero-retention"}[withZDR], func(t *testing.T) {
			currentZDR := zdr
			if !withZDR {
				var list struct {
					Data []json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(zdr, &list); err != nil {
					t.Fatal(err)
				}
				kept := list.Data[:0]
				for _, raw := range list.Data {
					var row struct {
						ModelID string `json:"model_id"`
					}
					if err := json.Unmarshal(raw, &row); err != nil {
						t.Fatal(err)
					}
					if row.ModelID != "typesafe/jev-1.13" {
						kept = append(kept, raw)
					}
				}
				list.Data = kept
				if currentZDR, err = json.Marshal(list); err != nil {
					t.Fatal(err)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/models":
					if r.URL.Query().Get("output_modalities") != "text,decisions,embeddings" || r.Header.Get("Authorization") != "Bearer synthetic-test-key" {
						t.Errorf("decisions discovery changed: %s", r.URL.String())
					}
					_, _ = w.Write(expanded)
				case "/v1/endpoints/zdr":
					_, _ = w.Write(currentZDR)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			target := Provider{Slug: "openrouter", BaseURL: server.URL + "/v1", APIKey: "synthetic-test-key"}
			models, err := Discover(context.Background(), server.Client(), target)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
			built, err := BuildSnapshot([]Discovery{{Provider: target, Models: models}}, attribution, Observations{}, at)
			if err != nil {
				t.Fatal(err)
			}
			var file snapshotFile
			if err := json.Unmarshal(built.Body, &file); err != nil {
				t.Fatal(err)
			}
			var jev []snapshotDeployment
			for _, deployment := range file.Deployments {
				if strings.HasPrefix(deployment.UpstreamModelID, "typesafe/") {
					jev = append(jev, deployment)
				}
			}
			if !withZDR {
				if len(jev) != 0 {
					t.Fatalf("Jev published without a zero-retention endpoint: %+v", jev)
				}
				return
			}
			want := snapshotDeployment{
				DeploymentID: "dep_openrouter_typesafe_jev_1_13_20260917_observed_2026_10_19", Provider: "openrouter",
				ModelReference: "typesafe/jev-1.13@observed-2026-10-19", UpstreamModelID: "typesafe/jev-1.13-20260917", Current: true,
			}
			if len(jev) != 1 || jev[0].DeploymentID != want.DeploymentID || jev[0].ModelReference != want.ModelReference || jev[0].UpstreamModelID != want.UpstreamModelID || !jev[0].Current {
				t.Fatalf("Jev deployment = %+v, want %+v", jev, want)
			}
		})
	}
}

func TestDecisionDiscoveryDoesNotChangeOtherProviderQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer synthetic-test-key" {
			t.Errorf("other provider request changed: %s", r.URL.String())
		}
		_, _ = io.Copy(w, strings.NewReader(`{"data":[{"id":"ordinary-model"}]}`))
	}))
	defer server.Close()
	models, err := Discover(context.Background(), server.Client(), Provider{Slug: "custom-compatible", BaseURL: server.URL + "/v1", APIKey: "synthetic-test-key"})
	if err != nil || len(models) != 1 {
		t.Fatalf("ordinary provider: %v", err)
	}
}
