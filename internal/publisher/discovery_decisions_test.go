package publisher

import (
	"bytes"
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

	"github.com/OxyHQ/Kaana/internal/contract"
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

func TestDecisionDiscoveryAuthenticatedListToPrivateSnapshot(t *testing.T) {
	text := decisionsFixture(t, "text")
	expanded := decisionsFixture(t, "text-decisions")
	zdr := decisionsFixture(t, "zdr")
	for _, missingZDR := range []bool{false, true} {
		name := "exact-private-candidate"
		if missingZDR {
			name = "no-zdr-refused"
		}
		t.Run(name, func(t *testing.T) {
			currentZDR := zdr
			if missingZDR {
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
				var err error
				currentZDR, err = json.Marshal(list)
				if err != nil {
					t.Fatal(err)
				}
			}
			listReads, zdrReads := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected method %s", r.Method)
					w.WriteHeader(405)
					return
				}
				switch r.URL.Path {
				case "/v1/models":
					listReads++
					if r.Header.Get("Authorization") != "Bearer synthetic-test-key" {
						t.Error("model discovery lost authentication")
						w.WriteHeader(401)
						return
					}
					if r.URL.Query().Get("output_modalities") == "text,decisions" {
						_, _ = w.Write(expanded)
					} else {
						_, _ = w.Write(text)
					}
				case "/v1/endpoints/zdr":
					zdrReads++
					if r.Header.Get("Authorization") != "" {
						t.Error("credential sent to public ZDR list")
					}
					_, _ = w.Write(currentZDR)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			discoveries, permit := privateFixture(t)
			target := discoveries[0].Provider
			target.BaseURL = server.URL + "/v1"
			models, err := Discover(context.Background(), server.Client(), target)
			if err != nil {
				t.Fatal(err)
			}
			if listReads != 1 || zdrReads != 1 {
				t.Fatalf("expected one authenticated list plus public ZDR, got %d/%d", listReads, zdrReads)
			}
			discoveries[0] = Discovery{Provider: target, Models: models}
			allowed := func(contract.DeploymentID, contract.ProviderSlug) (Withholding, bool) { return Withholding{}, false }
			at := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
			candidate, err := scopedCandidate(discoveries, permit, at, allowed, false)
			if missingZDR {
				if err == nil || candidate != nil {
					t.Fatal("private candidate admitted without ZDR")
				}
				return
			}
			if err != nil || candidate == nil {
				t.Fatalf("authenticated decisions discovery omitted exact private candidate: %v", err)
			}
			if candidate.Current || candidate.ScopedExecution == nil || !candidate.ScopedExecution.Equal(&permit.Audience) {
				t.Fatal("private candidate authority changed")
			}
			attribution, err := LoadAttribution("../../configs/model-attribution.json")
			if err != nil {
				t.Fatal(err)
			}
			ordinary, err := BuildSnapshot(discoveries, attribution, Observations{}, at)
			if err != nil {
				t.Fatal(err)
			}
			// Remove only the 13 new IDs using the retained default catalogue, then
			// compare complete rendered ordinary inventory bytes, not just counts.
			var defaultList struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := json.Unmarshal(text, &defaultList); err != nil {
				t.Fatal(err)
			}
			ids := map[string]bool{}
			for _, row := range defaultList.Data {
				ids[row.ID] = true
			}
			oldModels := []DiscoveredModel{}
			added := 0
			for _, model := range models {
				if ids[model.UpstreamModelID] {
					oldModels = append(oldModels, model)
				} else {
					added++
					if _, ok := attribution.ModelLine(target.Slug, model.UpstreamModelID); ok {
						t.Fatalf("new decision ID gained ordinary attribution: %s", model.UpstreamModelID)
					}
				}
			}
			if len(oldModels) != 466 || added != 13 {
				t.Fatalf("retained fixture changed: %d old/%d new", len(oldModels), added)
			}
			before, err := BuildSnapshot([]Discovery{{Provider: target, Models: oldModels}}, attribution, Observations{}, at)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before.Body, ordinary.Body) {
				t.Fatal("ordinary inventory changed when adding decisions")
			}
			private, err := buildSnapshotWithholding(discoveries, attribution, Observations{}, at, allowed, false, permit)
			if err != nil {
				t.Fatal(err)
			}
			if private.Deployments != ordinary.Deployments+1 || private.PrivatePublicationOmitted {
				t.Fatal("expected exactly one additional scoped route")
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
