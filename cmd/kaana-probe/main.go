package main

import (
	"net/http"
	"os"
	"time"
)

const livezURL = "http://127.0.0.1:8080/livez"

func main() {
	client := &http.Client{Timeout: 2 * time.Second}
	if !waitHealthy(client, livezURL, time.Now().Add(90*time.Second), time.Second) {
		_, _ = os.Stderr.WriteString("candidate /livez did not become healthy within 90s\n")
		os.Exit(1)
	}
}

// waitHealthy polls url until it answers 200 or the deadline passes. The canary
// has no shell, so this binary is its only health check: it must succeed only
// on a real 200, and give up when the deadline passes.
func waitHealthy(client *http.Client, url string, deadline time.Time, every time.Duration) bool {
	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(every)
	}
	return false
}
