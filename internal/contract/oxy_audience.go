package contract

// OxyAudience is who an Oxy service token Kaana mints is FOR: the `aud` Oxy
// writes into it and checks on every route (Oxy's serviceTokenAudiences.ts,
// Kaana ADR 0002). Kaana names it at the mint; Oxy decides everything else.
type OxyAudience string

const (
	// OxyAPIAudience is Oxy's own API: the BYOK validation callback, activity,
	// the publishers. Oxy's default when a mint names none.
	OxyAPIAudience OxyAudience = "oxy-api"
	// OxyKaanaControlAudience is Oxy's /internal/kaana/v1/*: the grant API every
	// https://kaana.ai/v1 request asks before it runs, and settlement delivery.
	// Only the Kaana application may hold it, and Oxy refuses an oxy-api token
	// there with 401 kaana_workload_required.
	OxyKaanaControlAudience OxyAudience = "oxy-api:kaana-control"
)
