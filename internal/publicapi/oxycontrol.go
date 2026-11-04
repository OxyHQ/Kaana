package publicapi

import (
	"context"
	"net/http"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// OxyControl is Kaana's way into Oxy's /internal/kaana/v1/*: its own workload
// token for that API (Oxy ADR 0026, audience oxy-api:kaana-control), never the
// caller's credential, and the URL of each route.
//
// The token and where it is sent come from ONE value, and the URL is derived
// from a fixed route rather than configured: *oxyvalidation.Reporter implements
// both from the single Oxy origin it validated, the one that mints the token.
// A separately configured URL would be a second place deciding where Kaana's
// credential may go, and the only one nothing checked.
type OxyControl interface {
	ControlToken(ctx context.Context) (string, error)
	ControlURL(endpoint contract.OxyKaanaControlEndpoint) string
}

// controlClient is the client every call carrying the control token goes
// through. It follows no redirect: Go forwards Authorization on a redirect to
// the same domain or any subdomain of it, and resends the body (the request,
// and a settlement with Kaana's signature) wherever a 307 or 308 points. A 3xx
// is therefore Oxy's answer, read as the refusal it is, and the control origin
// stays the only place the token and those bytes go.
func controlClient(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	copied := *client
	copied.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copied
}
