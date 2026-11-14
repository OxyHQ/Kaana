package publicapi

// PinnedReaderSets are the contract sets the deployed releases pinned in
// packages/sdk/src/__tests__/compatibility.test.ts state in
// Kaana-Contract-Version, "" for one that states none (`@kaana.ai/sdk` 1.0.1):
// `@kaana.ai/sdk` 2.0.0 (5.0.0), 3.3.0 (8.0.0, Clarity), 3.5.0 (8.2.0, Gemies
// and Mention), 3.16.0 (8.15.0, Syra) and 3.17.0 (8.16.0). The catalogue is
// recorded as each of them receives it; that suite fails for a pinned release
// whose set is not listed here.
var PinnedReaderSets = []string{"", "5.0.0", "8.0.0", "8.2.0", "8.15.0", "8.16.0"}

// PinnedReaderKey is a pinned set's key in the recorded fixtures.
func PinnedReaderKey(set string) string {
	if set == "" {
		return "unstated"
	}
	return set
}
