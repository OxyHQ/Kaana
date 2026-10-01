package provider

import (
	"crypto/tls"
	"net/http"
	"time"
)

// NewSingleAttemptHTTPClient confines a scoped exchange to a fresh HTTP/1
// connection with no proxy, redirect, idle reuse or HTTP/2 replay machinery.
// The caller must also clear Request.GetBody before sending its POST.
func NewSingleAttemptHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableKeepAlives = true
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	transport.Protocols.SetHTTP2(false)
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
