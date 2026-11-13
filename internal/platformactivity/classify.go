package platformactivity

import (
	"regexp"
	"strings"
)

// Activity types, the closed set Oxy's collector accepts.
const (
	typeIdentity = "identity"
	typeAI       = "ai"
	typeMedia    = "media"
	typePlatform = "platform"
)

// excludedSegments are the first path segments (after an optional `api`) whose
// requests are never reported: liveness and readiness probes, status pages and
// the dashboard transport. It is a superset of the exclusions in Oxy's
// collector (packages/telemetry/src/collector.ts): health, ready, live,
// platform-stats, platform-activity, platform-infrastructure and infra-status,
// plus the probe spellings Kaana and its load balancer use (livez, healthz,
// readyz, status).
var excludedSegments = map[string]struct{}{
	"health": {}, "healthz": {}, "ready": {}, "readyz": {}, "live": {}, "livez": {}, "status": {},
	"platform-stats": {}, "platform-activity": {}, "platform-infrastructure": {}, "infra-status": {},
}

// excludedPrefixes are path prefixes that carry collection itself, service
// token minting and the edge's own endpoints. Reporting them
// would make the collector observe its own traffic.
var excludedPrefixes = []string{
	"/internal/activity",
	"/internal/v1/health",
	"/auth/service-token",
	"/cdn-cgi/",
}

// Excluded reports whether a request path is control or probe traffic that
// never becomes a flow. Inbound and outbound observation share this one table.
func Excluded(path string) bool {
	for _, prefix := range excludedPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	trimmed := strings.TrimPrefix(strings.TrimPrefix(path, "/"), "api/")
	segment, _, _ := strings.Cut(trimmed, "/")
	_, excluded := excludedSegments[segment]
	return excluded
}

var versionSegment = regexp.MustCompile(`^v\d+$`)

// activityTypes maps the first meaningful path segment to a type. It follows
// the shape of Oxy's classifier (leading `api` and version segments are
// skipped, an unknown segment is `platform`) with Kaana's own surface in place
// of Oxy's: Kaana serves inference, so its inference routes are `ai` (Oxy's
// table would call /v1/messages `communication`) and its audio and image
// routes are `media`. Credential control, settlements and Oxy's
// /internal/kaana control routes are `platform`; identity resolution is
// `identity`.
var activityTypes = map[string]string{
	"inference": typeAI, "chat": typeAI, "completions": typeAI, "messages": typeAI,
	"responses": typeAI, "embeddings": typeAI, "rerank": typeAI, "decisions": typeAI,
	"models": typeAI, "generations": typeAI, "power-levels": typeAI, "realtime": typeAI,
	"audio": typeMedia, "images": typeMedia,
	"identities": typeIdentity,
}

// activityType derives the type from a path alone. The path is read, never
// stored: only the resulting type enters a flow.
func activityType(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, skipped := range []func(string) bool{
		func(s string) bool { return s == "internal" },
		func(s string) bool { return s == "kaana" },
		func(s string) bool { return s == "api" },
		versionSegment.MatchString,
	} {
		if len(parts) > 1 && skipped(parts[0]) {
			parts = parts[1:]
		}
	}
	if kind, ok := activityTypes[parts[0]]; ok {
		return kind
	}
	return typePlatform
}
