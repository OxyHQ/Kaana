package provider_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/OxyHQ/Kaana/internal/provider"
)

func TestCredentialClientRefusesCrossOriginRedirects(t *testing.T) {
	var destinationRequests atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		destinationRequests.Add(1)
	}))
	t.Cleanup(destination.Close)

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, destination.URL, http.StatusFound)
	}))
	t.Cleanup(source.Close)

	request, err := http.NewRequest(http.MethodGet, source.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("Authorization", "Bearer provider-secret")
	request.Header.Set("x-api-key", "provider-secret")
	response, err := provider.RefuseRedirects(nil).Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("response body close: %v", err)
		}
	})
	if response.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want the original redirect", response.StatusCode)
	}
	if destinationRequests.Load() != 0 {
		t.Fatal("credential-bearing request followed a cross-origin redirect")
	}
}
