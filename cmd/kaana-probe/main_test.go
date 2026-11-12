package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestTheProbeSucceedsOnlyOnARealOK(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Starting up: two refusals, then healthy.
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	if !waitHealthy(server.Client(), server.URL, time.Now().Add(5*time.Second), time.Millisecond) {
		t.Fatal("the probe gave up on a task that became healthy")
	}
	if calls.Load() != 3 {
		t.Errorf("it polled %d times, want the two refusals and the OK", calls.Load())
	}
}

func TestTheProbeNeverTreatsAnythingButOKAsHealthy(t *testing.T) {
	for name, status := range map[string]int{"redirect": http.StatusFound, "no content": http.StatusNoContent, "server error": http.StatusInternalServerError} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		healthy := waitHealthy(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
			server.URL, time.Now().Add(100*time.Millisecond), 10*time.Millisecond)
		server.Close()
		if healthy {
			t.Errorf("%s was accepted as healthy", name)
		}
	}
}

func TestTheProbeGivesUpWhenNothingIsListening(t *testing.T) {
	started := time.Now()
	if waitHealthy(&http.Client{Timeout: 50 * time.Millisecond}, "http://127.0.0.1:1/livez", time.Now().Add(150*time.Millisecond), 10*time.Millisecond) {
		t.Fatal("the probe reported a dead address as healthy")
	}
	if time.Since(started) > 2*time.Second {
		t.Error("the probe outlived its deadline")
	}
}
