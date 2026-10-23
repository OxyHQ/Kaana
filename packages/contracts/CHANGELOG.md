# Changelog

## 8.0.1

Require Zod 4.6.7 or newer in major 4 to include the oversized-array validation
fix (GHSA-mrhx-c7xc-m3w5). String bounds retain the existing UTF-16 code-unit
contract through explicit per-schema checks; the Go descriptor, wire shapes
and public limits remain unchanged.

## 8.0.0

Authenticated and grant principals are discriminated by authority. First-party
principals carry no scopes; external principals retain explicit scope arrays.
Consumers must narrow the union before reading `scopes` or `inferenceScopes`.
Historical signed records without an authority field remain external.

The Kaana reader accepts existing external grants and the new first-party shape.
Deploy that reader before enabling Oxy's first-party emitter. Signature,
audience, billing, route and account boundaries remain enforced.
