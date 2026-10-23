# Changelog

## 8.0.0

Authenticated and grant principals are discriminated by authority. First-party
principals carry no scopes; external principals retain explicit scope arrays.
Consumers must narrow the union before reading `scopes` or `inferenceScopes`.
Historical signed records without an authority field remain external.

The Kaana reader accepts existing external grants and the new first-party shape.
Deploy that reader before enabling Oxy's first-party emitter. Signature,
audience, billing, route and account boundaries remain enforced.
