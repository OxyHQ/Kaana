package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type metadataTransport func(*http.Request) (*http.Response, error)

func (f metadataTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRealtimePlacementUsesOnlyItsPrivateContainer(t *testing.T) {
	valid := `{"Name":"kaana","Networks":[{"NetworkMode":"awsvpc","IPv4Addresses":["10.2.3.4"]}]}`
	const endpoint = "http://169.254.170.2/v4/own-container"
	for name, body := range map[string]string{
		"valid":             valid,
		"another container": strings.ReplaceAll(valid, `"kaana"`, `"inventory-sync"`),
		"outside VPC":       strings.ReplaceAll(valid, "10.2.3.4", "10.3.3.4"),
		"public":            strings.ReplaceAll(valid, "10.2.3.4", "1.2.3.4"),
		"bridge":            strings.ReplaceAll(valid, "awsvpc", "bridge"),
		"ambiguous":         strings.ReplaceAll(valid, `"10.2.3.4"`, `"10.2.3.4","10.2.3.5"`),
		"oversized":         valid + strings.Repeat(" ", 65536),
	} {
		t.Run(name, func(t *testing.T) {
			transport := metadataTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != endpoint || len(r.Header) != 0 {
					t.Fatal("metadata request changed destination or carried credentials")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})
			address, subnet, err := readRealtimePlacement(context.Background(), "10.2.0.0/16", endpoint, transport)
			if name == "valid" {
				if err != nil || address.String() != "10.2.3.4" || subnet.String() != "10.2.0.0/16" {
					t.Fatalf("placement = %s %s %v", address, subnet, err)
				}
			} else if err == nil {
				t.Fatal("invalid placement accepted")
			}
		})
	}
	for _, invalid := range []string{"http://evil.test/v4/own", "http://169.254.170.2/v4/own/task", "http://user@169.254.170.2/v4/own", endpoint + "?target=x", "http://169.254.170.2/v4/%2e%2e"} {
		_, _, err := readRealtimePlacement(context.Background(), "10.2.0.0/16", invalid, metadataTransport(func(*http.Request) (*http.Response, error) { t.Fatal("invalid metadata URL fetched"); return nil, nil }))
		if err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
	for _, cidr := range []string{"", "0.0.0.0/0", "127.0.0.0/8", "10.2.1.0/16"} {
		_, _, err := readRealtimePlacement(context.Background(), cidr, endpoint, nil)
		if err == nil {
			t.Fatalf("accepted %s", cidr)
		}
	}
	calls := 0
	_, _, err := readRealtimePlacement(context.Background(), "10.2.0.0/16", endpoint, metadataTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"http://evil.test"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	if err == nil || calls != 1 {
		t.Fatal("metadata redirect followed")
	}
}
