package provider

import "net/http"

// RefuseRedirects clones a client and prevents requests carrying upstream
// credentials from following a provider-controlled Location header. Go does
// not strip every authentication header on every cross-origin or downgrade
// redirect, so the only safe implicit redirect policy is none.
func RefuseRedirects(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &clone
}
