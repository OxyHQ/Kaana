package main

import (
	"net/http"
	"os"
	"time"
)

func main() {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get("http://127.0.0.1:8080/livez")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(time.Second)
	}
	_, _ = os.Stderr.WriteString("candidate /livez did not become healthy within 90s\n")
	os.Exit(1)
}
