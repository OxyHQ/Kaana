package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/realtime"
)

func realtimePeers(ctx context.Context, store *credentialstore.Postgres) (*realtime.PeerConfig, error) {
	cidr, metadata := os.Getenv("KAANA_REALTIME_PEER_VPC_CIDR"), os.Getenv("ECS_CONTAINER_METADATA_URI_V4")
	if cidr == "" && metadata == "" {
		return nil, nil // A local, single-process server has no private peer network.
	}
	listen := os.Getenv("KAANA_ADDR")
	if listen != "" && listen != ":8080" && listen != "0.0.0.0:8080" {
		return nil, errors.New("realtime peers require serving port 8080")
	}
	// The metadata link-local endpoint must never traverse an HTTP proxy.
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext}
	defer transport.CloseIdleConnections()
	address, subnet, err := readRealtimePlacement(ctx, cidr, metadata, transport)
	if err != nil {
		return nil, err
	}
	return &realtime.PeerConfig{Directory: credentialstore.RealtimeOwners{Store: store}, Address: address, Subnet: subnet}, nil
}

// Placement comes from this container's link-local ECS metadata, never a
// client, a forwarded header, a public hostname, or an arbitrary task address.
func readRealtimePlacement(ctx context.Context, cidr, metadata string, transport http.RoundTripper) (netip.Addr, netip.Prefix, error) {
	fail := func() (netip.Addr, netip.Prefix, error) {
		return netip.Addr{}, netip.Prefix{}, errors.New("realtime peers require exact private VPC and own ECS awsvpc container metadata")
	}
	subnet, err := netip.ParsePrefix(cidr)
	if err != nil || !subnet.Addr().Is4() || !subnet.Addr().IsPrivate() || subnet != subnet.Masked() || (subnet.Bits() < 16 || subnet.Bits() > 28) {
		return fail()
	}
	endpoint, err := url.Parse(metadata)
	if err != nil || endpoint.Scheme != "http" || endpoint.Host != "169.254.170.2" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || !strings.HasPrefix(endpoint.Path, "/v4/") || strings.Count(endpoint.Path, "/") != 2 || endpoint.RawPath != "" {
		return fail()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fail()
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("metadata redirects are refused") }}
	response, err := client.Do(request)
	if err != nil {
		return fail()
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fail()
	}
	var container struct {
		Name     string
		Networks []struct {
			NetworkMode   string
			IPv4Addresses []string
		}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(body) > 64<<10 || json.Unmarshal(body, &container) != nil || container.Name != "kaana" || len(container.Networks) != 1 || container.Networks[0].NetworkMode != "awsvpc" || len(container.Networks[0].IPv4Addresses) != 1 {
		return fail()
	}
	address, err := netip.ParseAddr(container.Networks[0].IPv4Addresses[0])
	if err != nil || !address.Is4() || !address.IsPrivate() || !subnet.Contains(address) {
		return fail()
	}
	return address, subnet, nil
}
