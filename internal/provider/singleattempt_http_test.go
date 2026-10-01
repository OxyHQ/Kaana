package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestSingleAttemptHTTPUsesOneHTTP1ExchangeAndRefusesRedirects(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.ProtoMajor != 1 || r.TLS == nil || r.TLS.NegotiatedProtocol != "http/1.1" {
			t.Errorf("wrong negotiated protocol %s", r.Proto)
		}
		http.Redirect(w, r, "/followed", http.StatusTemporaryRedirect)
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client := NewSingleAttemptHTTPClient()
	defer client.CloseIdleConnections()
	transport := client.Transport.(*http.Transport)
	transport.TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, bytes.NewReader([]byte("synthetic")))
	if err != nil {
		t.Fatal(err)
	}
	request.GetBody = nil
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusTemporaryRedirect || calls.Load() != 1 {
		t.Fatal("redirect/replay", calls.Load())
	}
}

func TestSingleAttemptHTTPDoesNotResendAfterUncertainConnection(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = connection.Close()
	}))
	defer server.Close()
	client := NewSingleAttemptHTTPClient()
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, bytes.NewReader([]byte("synthetic")))
	if err != nil {
		t.Fatal(err)
	}
	request.GetBody = nil
	response, err := client.Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || calls.Load() != 1 {
		t.Fatal("uncertain send repeated", err, calls.Load())
	}
}
