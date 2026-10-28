# Changelog

## 3.5.0

Depend on contracts 8.2.0: the client states contract set 8.2.0, whose image
rules (Kaana ADR 0007) Kaana applies to an `image` part: inline base64 only,
PNG, JPEG, WebP or GIF, at most 3,750,000 bytes each and 20 a request. The
request shapes are unchanged.

## 3.4.0

Depend on contracts 8.1.0: a realtime session parses `transcript.interim`, the
provisional text a `transcription` session sends when opened with
`inputAudioTranscription.interimResults: true` (Kaana ADR 0006), and
`realtime.connect` refuses that flag on any other kind before dialling. The
testing fake emits it like any other event. Nothing else changes.

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
