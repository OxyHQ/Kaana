# Changelog

## 3.3.0

Add `kaanaForOxyService` for own HTTP and realtime callers. The supplied shared
Oxy workload transport manages authentication, destination binding and refresh;
the SDK preserves its retries, streaming and realtime resume credential.
Inference attribution is immutable and remains app-paid accounting. Existing
external credential clients are unchanged. Requires contracts 8.0.1 and its
patched Zod dependency; the factory host needs Oxy core 7 or newer.

## 3.2.1

Depend on contracts 8.0.1, which requires patched Zod 4.6.7 or newer within
major 4. The SDK public API and wire behavior are unchanged; the contract
retains its UTF-16 string bounds and UTC timestamp grammar.
