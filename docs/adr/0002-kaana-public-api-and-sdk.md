# ADR 0002 — Kaana sirve su API pública `https://kaana.ai/v1` y publica su propio SDK; Oxy autoriza, reserva y liquida

- Status: proposed (borrador para revisión del dueño; nada implementado)
- Date: 2026-10-15
- Decided by: el dueño de Oxy (decisión de 2026-10-15; este documento la
  desarrolla, no la reabre)
- Supersedes, al aceptarse: Oxy ADR 0006 (fila «Public API edge and developer
  experience | Oxy» y «Kaana no autoriza clientes»), Oxy ADR 0015 en lo que
  firma (el sobre deja de llevar la petición y pasa a ser una concesión), Alia
  ADR 0001 («Alia neither signs nor calls Kaana directly», «uses the published
  `OxyInferenceClient`») y la frase de Alia ADR 0010 «generic inference is
  Kaana through Oxy». Ver «Qué queda sustituido».
- Counterparts por escribir (fase 0): Oxy ADR 0039 (API interna de
  autorización y liquidación; retirada de `@oxy.so/core/inference` y del borde
  `api.oxy.so/v1`) y Alia ADR 0016 (Alia consume `@kaana.ai/sdk`).
- Compatible con: Kaana ADR 0001 / Oxy ADR 0038 (Kaana elige proveedor y clave,
  decisión 2026-10-12), Oxy ADR 0011 (origen único `https://kaana.ai`), Oxy
  ADR 0016 (sin persistencia de payloads), Oxy ADR 0026 (servicios propios se
  autentican como workloads), Oxy ADR 0035 (introspección `oxy_sk_` por un
  servidor de recursos).

## Contexto

La decisión del dueño (2026-10-15): Kaana es un proveedor de inferencia como
OpenRouter u OpenAI, para terceros y para Oxy/Alia. Su API pública es
`https://kaana.ai/v1` y tiene su propio SDK de TypeScript. Oxy se queda con
identidad, Oxy Console, las claves `oxy_sk_…`, facturación y límites. El corte
es limpio: `@oxy.so/core/inference` (`OxyInferenceClient`,
`createInferenceClient`) desaparece sin adaptador, sin deprecación y sin doble
camino; todos sus consumidores migran en el mismo corte.

Estado medido en `origin/main` de cada repo el 2026-10-15 (Kaana `c1132d1`,
OxyHQServices `c87e7f5da`, Alia `28541bbac`, oxy-infra `0a4ad6b`):

**Kaana no tiene API pública.** `internal/httpapi/server.go:3-5` («internal
signed routes and no customer-facing one»); rutas en `server.go:183-193`:
`POST /internal/v1/{inference,decisions}`, `GET /internal/v1/realtime`
(WebSocket), `GET /internal/v1/{health,models}`,
`POST /internal/v1/{models,deployments}/query`, validaciones de credenciales de
cliente, feeds de telemetría y `GET /livez` (la única sin firma). La
autenticación es un sobre Ed25519 de Oxy (`internal/edgeauth/edgeauth.go:13-35`,
cabeceras `X-Oxy-Kaana-{Key-Id,Timestamp,Signature}`, dominio
`oxy-kaana-envelope:v1`, ventana de 5 min sin nonce, `:87-94`); claves públicas
por env `KAANA_EDGE_PUBLIC_KEYS` (`cmd/kaana/main.go:78-90`). No hay JWT ni
JWKS en Kaana. `docs/architecture.md:7-17` y `:310-335` prohíben a Kaana
cuentas, credenciales de API de Oxy, ledger y consola, e «it does not authorize
a customer»; `docs/identity-and-routing.md:33-40` dice que todo acceso de
desarrollador pasa por el borde de Oxy.

**El borde público de inferencia es Oxy, en `api.oxy.so/v1`.**
`OxyHQServices/packages/api/src/server.ts:842` (`app.use('/v1',
inferenceEdgeRoutes)`), `:846` y `:861` (catálogo), `:357-360` (WebSocket
realtime). Rutas en `routes/inferenceEdge.ts`: `POST /v1/decisions` (`:879`),
`/v1/responses` (`:978`), `/v1/chat/completions` (`:1088`, dialecto OpenAI),
`/v1/audio/speech` (`:1234`), `/v1/images/generations` (`:1334`),
`GET /v1/generations/by-idempotency-key` (`:1418`), `/v1/generations/:id`
(`:1493`); `/v1/audio/transcriptions` no existe a propósito (`:1590-1646`).
Realtime: `routes/inferenceRealtime.ts:108-121`. Ciclo de vida en
`services/inferenceEdge.service.ts`: autenticar `authenticateEdgeCaller`
(`:439-531`; `oxy_sk_` solo si `INFERENCE_MACHINE_CREDENTIAL_AUTH`, `:430-435`;
service token JWT `aud: oxy-api`; **la sesión de usuario se rechaza**,
`:474-477`, y lo confirma `packages/console/src/hooks/use-playground.ts:10-16`),
scopes `inference:invoke` (`:1003-1008`), política (`:1063`), preflight
`attestDeployments` (`:1885-1902`), dimensionado del hold con
`estimatedInputTokens` y `maxOutputTokens` (`:1951`), `reserve`
(`inferenceLedger.service.ts:744`, llamado en `inferenceEdge.service.ts:2235`;
TTL 900 s, `:298`), `buildEnvelope` (`:3788`), `kaanaClient.execute`/`stream`
(`:2373`, `:2736`) y `settle` (`inferenceLedger.service.ts:951`, vía `:2497` o
`settleMeasured` `:3357`). Oxy liquida con el `usage_report` que Kaana envía en
el mismo SSE (`Kaana internal/httpapi/server.go:58-61`, `:415-424`).

**El cliente que se elimina.** `OxyHQServices/packages/core/src/inference/`
contiene solo `OxyInferenceClient.ts` (1042 líneas) e `index.ts`. Origen por
defecto `https://api.oxy.so` (`OxyInferenceClient.ts:93`); credencial `string |
(() => string | null | Promise<string | null>)` (`:103`); métodos `listModels`
(`:399`), `getModel` (`:424`), `listRoutingProfiles` (`:451`), `respond`
(`:466`), `decide` (`:481`), `speech` (`:505`, bufferiza), `stream` (`:582`),
`getGeneration` (`:690`), `getGenerationRecord` (`:705`),
`getGenerationRecordByIdempotencyKey` (`:722`); opciones `signal`,
`idempotencyKey`, `delegatedUserId` (`X-Oxy-User-Id`, `:180-199`, `:748-768`);
errores `OxyInferenceError` (`:283-311`) y `OxyInferenceProtocolError`
(`:321-330`). `createInferenceClient(oxy)` (`index.ts:40-49`) usa la sesión del
usuario, que el borde rechaza: hoy es código muerto en la práctica. Publicado en
`@oxy.so/core` 4.5.0, subpath `./inference` (`packages/core/package.json:74`).

**El contrato vive en Oxy y Kaana lo deriva.** La fuente es zod en
`OxyHQServices/packages/contracts/src/inference/` (23 módulos, contract set
`3.8.0`, `version.ts:103`; `@oxy.so/contracts` 4.12.0). Mezcla el sobre
interno (`request.ts:391-394`, `usage.ts:180`, realtime), shapes públicas
(`streamEvents.ts`, `errors.ts:49-80`, decisions, catálogo) y shapes que no son
de inferencia (facturación, `inbox.ts`, `aliaModelRelease`). Las shapes de
petición pública ni siquiera están ahí: viven en
`packages/api/src/schemas/inferenceEdge.schemas.ts:113-672`. Kaana genera
`internal/contract/descriptor.json` (265 shapes, 42 constantes) desde el
paquete **publicado** con `tools/contract/generate.mjs:1-32`
(`@oxy.so/kaana-contract-tools`, privado) y sus tests Go comparan contra él
campo a campo. El descriptor no emite TypeScript, zod ni JSON Schema.

**Audio hoy.** `audio_speech` es solo no-streaming
(`internal/contract/request.go:438-439`); Kaana recibe el archivo entero y lo
trocea en eventos `audio` de ≤ 49 152 bytes (`docs/elevenlabs.md:101-103`).
ElevenLabs, xAI y Deepgram sintetizan sin streaming; el único audio en
streaming es la salida `pcm` de `chat_completions` con `audioOutput`
(`request.go:433-434`). La transcripción existe en adaptadores
(`docs/openai-audio.md:42-55`, `docs/deepgram.md:10-24`) pero no en el borde.
El inventario (`internal/inventory/inventory.go:104-130`) lleva contexto,
salida máxima, modalidades, herramientas, esfuerzos, parámetros y precio de
lista; **no lleva voces, idiomas ni formatos**. Realtime: WebSocket con primer
frame firmado, solo sesiones `conversation` con claves de plataforma, y
liquidación exactamente una vez (`docs/realtime.md:30-69`, `:143-148`,
`:182-206`).

**El host.** oxy-infra: el apex `kaana.ai` es un CNAME **proxied por
Cloudflare** hacia un ALB público dedicado (`terraform-uswest2/app-kaana.tf:380-401`,
`idle_timeout = 900`), cuyo listener 443 reenvía **todo** al target group de
Kaana sin reglas de host ni de path (`:490-501`). El SG del ALB solo admite
rangos de Cloudflare (`:254-314`); no hay mTLS ni allow-list de Oxy. Oxy llama
por el mismo camino público (`app-services.tf:113`, `KAANA_BASE_URL=https://kaana.ai`;
la zona privada de Route 53 se eliminó, `app-kaana.tf:538-553`). Límite de
TTFB de Cloudflare asumido: 125 s (`app-kaana.tf:555-584`). Rate limiting:
Kaana documenta una regla Cloudflare solo para `/internal/v1/inference`
(`.github/scripts/cloudflare_rate_limit.py:21-39`). El servicio `kaana` corre
con `desired_count = 1` (`app-kaana.tf:961-1024`). `api.kaana.ai` **no es
Kaana**: es un host-header que enruta a oxy-api en el ALB compartido
(`app-services.tf:16-21`), marcado como temporal. Hoy `kaana.ai/v1/...` llega a
Kaana y devuelve 404.

## Decisión

1. **Kaana sirve `https://kaana.ai/v1` en su propio proceso Go.** No se mueve
   el borde de Oxy de host. El borde de Oxy se descompone: lo que decide quién
   puede gastar y cuánto (identidad, política de la app, límites, reserva,
   liquidación) se queda en Oxy como **API interna** que Kaana consulta; lo que
   toca la petición (parseo, validación, streaming, dialecto OpenAI,
   idempotencia de ejecución, audio, realtime) pasa a Kaana. El payload deja de
   pasar por Oxy.

   Por qué no mover el borde de Oxy a `kaana.ai/v1` (regla de ALB/Cloudflare
   apuntando a oxy-api): sería una API de Kaana escrita y desplegada por Oxy,
   con dos saltos y dos parsers de SSE por token, el WebSocket de realtime
   proxificado dos veces, el límite de 125 s de Cloudflare en ambos saltos y el
   sobre firmado transportando cada payload. Contradice ADR 0001 fase 2 («Oxy
   stops choosing deployments») y obligaría a que el `auto` por contenido
   siguiera en Oxy. El coste de nuestra opción es real y se acepta: Kaana pasa
   a ser un servicio expuesto a clientes (rate limiting por credencial, límite
   de cuerpo antes de autenticar, escalado horizontal, CORS).

2. **Oxy sigue siendo el único que decide quién gasta.** Kaana no valida
   credenciales por su cuenta salvo verificar firmas: cada petición
   facturable abre una **concesión** con
   `POST https://api.oxy.so/internal/kaana/v1/authorizations`, y cada concesión
   se cierra con `POST https://api.oxy.so/internal/kaana/v1/settlements`.
   - Kaana se autentica ante Oxy como workload (Oxy ADR 0026: firma
     `GetCallerIdentity` del rol de la tarea ECS → service token). Sin par de
     claves en SSM.
   - La autorización recibe la credencial del cliente **tal cual**, opaca
     (como la introspección de Oxy ADR 0035), más hechos sin contenido:
     operación, objetivo pedido (`publisher/model` o nivel de potencia),
     `estimatedInputTokens`, `maxOutputTokens`, caracteres o segundos de audio,
     `stream`, `Idempotency-Key`, `X-Oxy-User-Id` si viene, y el
     `sha256` del cuerpo normalizado. Nunca el prompt, el audio ni la salida.
   - Oxy resuelve el principal (clave `oxy_sk_` con secreto, tipo, estado,
     expiración, entorno y scopes; service token JWT; sesión de usuario de
     una app), aplica la política de la app y los límites, **reserva** con el
     ledger actual (`reserve`, mismo dimensionado) y devuelve una **concesión
     firmada** Ed25519 con un dominio nuevo `oxy-kaana-grant:v1` y las mismas
     claves que hoy (`KAANA_EDGE_PUBLIC_KEYS`). La concesión liga
     `requestId`, `reservationId`, principal opaco, `bodySha256`, objetivo,
     límites (`maxPricePerRequest`, `maxFundingTier`, retención y privacidad)
     y, mientras ADR 0001 fase 2 no esté hecha, las `authorizedRoutes` que
     Oxy firma hoy. Expira a los 60 s si no empieza la ejecución.
   - Kaana verifica la firma y el `bodySha256` contra el cuerpo que tiene y
     ejecuta dentro de la concesión. Una petición sin concesión válida no
     llega a ningún proveedor.
   - **Liquidación exactamente una vez por concesión.** Al terminar (éxito,
     error, cancelación o cierre de sesión realtime) Kaana escribe el
     `normalizedUsageReport` en una **bandeja de salida durable en su
     PostgreSQL** dentro de la misma transacción que sus `provider_cost_events`,
     y un worker lo entrega a `/settlements`, firmado por Kaana
     (dominio `kaana-oxy-settlement:v1`, clave privada nueva de Kaana en
     SSM; Oxy guarda la pública). Idempotente por `reservationId`. Oxy expira
     las reservas sin liquidar con el `expireReservations` existente
     (`inferenceLedger.service.ts:1247`) y lee la bandeja con cursor para
     reconciliar, igual que ya lee `/internal/v1/provider-telemetry/attempts`.
     Kaana sigue sin enviar ni conocer importes de cliente (`docs/cost.md:11-20`).
   - Recibos (`GET /v1/generations/:id`, `/by-idempotency-key`): los sirve
     Kaana como lectura autorizada por Oxy (`authorizations` con operación
     `receipt.read` y scope `inference:usage:read`), con el importe que Oxy
     devuelve en la respuesta de autorización. El ledger sigue en Oxy.

3. **Qué pasa con `kaana.ai/internal/v1`.** Se **eliminan** las rutas que
   transportan peticiones de clientes: `POST /internal/v1/inference`,
   `POST /internal/v1/decisions` y `GET /internal/v1/realtime`, con los sobres
   v1–v4 y la verificación `oxy-kaana-envelope:v1`. Sin doble camino: Oxy no
   conserva ningún modo de reenviar una petición. **Se quedan** como plano de
   control Oxy→Kaana, firmadas igual que hoy: `GET /internal/v1/{health,models}`,
   `POST /internal/v1/{models,deployments}/query`, validaciones y mutaciones de
   credenciales de cliente y de plataforma, y los feeds de telemetría. Se
   añade `GET /internal/v1/settlements` (cursor) para la reconciliación. Kaana
   ya responde 401 a `/internal/*` sin firma; lo nuevo en el borde es un rate
   limit de Cloudflare separado para `/v1/*` y para `/internal/*`.

4. **Superficie pública `https://kaana.ai/v1`.** Una sola versión, sin alias:

   | Método y ruta | Notas |
   |---|---|
   | `GET /v1/models`, `GET /v1/models/{publisher}/{model}` | catálogo con modalidades, voces, idiomas, límites, formatos y precio de lista de Oxy |
   | `GET /v1/power-levels` | los niveles (`auto` … `ultra`) visibles para la credencial; sustituye a `/v1/models/routing-profiles` |
   | `POST /v1/responses` | nativo; SSE si `stream: true` |
   | `POST /v1/chat/completions` | compatible con el SDK de OpenAI (`baseURL: 'https://kaana.ai/v1'`, `apiKey: 'oxy_sk_…'`) |
   | `POST /v1/embeddings` | ya en el contrato (`internal/contract/embeddings.go`) |
   | `POST /v1/audio/speech` | `stream: false` → archivo; `stream: true` → audio en streaming (decisión 6) |
   | `POST /v1/audio/transcriptions` | multipart, ≤ 20 MiB, síncrono |
   | `POST /v1/images/generations` | se conserva lo que hoy sirve Oxy |
   | `POST /v1/decisions` | familia dormida (`docs/decisions.md:3-10`); conserva sus revisiones pendientes |
   | `GET /v1/generations/{id}`, `GET /v1/generations?idempotency_key=` | recibo de uso y coste |
   | `GET /v1/realtime?model=` | WebSocket; primer frame `session.open`/`session.resume` autenticado por la credencial del handshake |
   | `GET /livez` | sin cambios |

   Reglas del protocolo que se conservan de hoy: el estado HTTP solo decide
   antes del primer byte; después, todo fallo es el evento terminal `error`
   del stream, tipado, nunca un relleno con `finish_reason: "stop"`
   (`server.go:15-27`). Ids de modelo `publisher/model[@revision]` sin
   sustitución; niveles sin `/`. Nombres de operador y `deploymentId` nunca
   salen en la API pública. Cabeceras de respuesta `Kaana-Request-Id` y
   `Kaana-Contract-Version`.

5. **Credenciales aceptadas en `kaana.ai/v1`** (las tres resueltas por Oxy en
   `authorizations`; Kaana no las interpreta):
   - **Clave de Oxy Console** `Authorization: Bearer oxy_sk_…`. Terceros y
     herramientas OpenAI. La paga la aplicación de la clave.
   - **Service token de Oxy** (JWT `type: 'service'`). Servicios propios
     (Alia, Mention, Homiio, Inbox en Oxy). Admite `X-Oxy-User-Id` para
     atribución delegada, como hoy. Para aceptarlo hace falta que Oxy emita
     tokens con audiencia `kaana.ai` (hoy `aud: oxy-api`,
     `middleware/serviceToken.ts:189-201`).
   - **Sesión de usuario de una app Oxy** (JWT de sesión con `clientId`). Es
     nueva: el borde actual la rechaza. Exige que la app tenga
     `inference:invoke` consentido por el usuario (la etiqueta ya existe,
     `packages/services/src/ui/components/OxyConsentScreen.tsx:125-131`) y
     abre CORS en `kaana.ai/v1` para los orígenes registrados de la app.
   - `alia_sk_*` sigue retirado; Kaana no emite claves propias.

6. **`speechStream`: audio en streaming chunked.** `POST /v1/audio/speech` con
   `stream: true` y `stream_format`:
   - `"audio"` (por defecto, compatible con OpenAI): `200`,
     `Content-Type: audio/mpeg` o `audio/pcm;rate=24000`,
     `Transfer-Encoding: chunked`, bytes crudos. Un fallo antes del primer byte
     es un estado HTTP con cuerpo de error; uno posterior corta la conexión y
     el recibo queda en `GET /v1/generations/{id}` con su código.
   - `"sse"` (el que usa el SDK): eventos `speech.audio.delta` (base64),
     `speech.audio.done` (con `usage`) y `error`, para que un fallo a mitad de
     audio llegue tipado.
   El primer byte sale en cuanto el adaptador lo produce. Hoy ningún adaptador
   de síntesis hace streaming real (Kaana trocea el archivo completo), así que
   la latencia al primer byte solo baja cuando se escriba el adaptador de
   streaming de cada proveedor (ElevenLabs `/stream`, OpenAI `stream_format`).
   Eso es trabajo de adaptadores bajo `docs/adapters.md`, no del SDK. La
   rama `feat/speech-stream` que ya existe en este repo (otra sesión, sin
   commits sobre `main` a 2026-10-15) se coordina con esta decisión.

7. **Catálogo con voces, idiomas y formatos.** `inventory.observed` gana
   `voices[]` (`id`, `name`, `languages`, `gender?`), `languages[]` (BCP 47)
   y `audioFormats{input,output}`, publicados por el publisher y validados por
   `inventory.Parse`. `GET /v1/models` los sirve junto a `contextTokens`,
   `maxOutputTokens`, modalidades, `apiFormats`, `streaming`, `realtime` y
   parámetros aceptados. El **precio** que ve el cliente es lo que Oxy cobra,
   no el coste de Kaana: Oxy publica una lista de precios firmada que Kaana
   cachea y sirve tal cual; Kaana nunca calcula un importe de cliente. La
   visibilidad por app (niveles y modelos permitidos por la política) sale de
   una autorización sin reserva (`operation: 'catalogue.read'`) cacheada por
   credencial 60 s.

8. **El contrato de inferencia se muda a Kaana como `@kaana.ai/contracts`.**
   - Fuente: zod 4 en `packages/contracts/src/` de **este repo**. Contiene las
     shapes públicas (peticiones y respuestas de `/v1/*`, eventos de stream,
     errores, catálogo público, realtime, speech, transcripción, decisions,
     recibos) y, en el subpath `@kaana.ai/contracts/internal`, las de
     Oxy↔Kaana (concesión, liquidación, bandeja de salida, plano de control).
     Las shapes de petición pública que hoy viven en
     `OxyHQServices/packages/api/src/schemas/inferenceEdge.schemas.ts` se
     mudan aquí: el contrato público pasa a ser uno solo.
   - Se quedan en `@oxy.so/contracts`: facturación, ledger, precios, política
     de enrutado como recurso de Console, entitlements, `inbox.ts` (es producto
     de Oxy, `/email/ai/*`) y `aliaModelRelease`. El directorio
     `packages/contracts/src/inference/` desaparece de Oxy en el corte.
   - `tools/contract/generate.mjs` deriva `descriptor.json` del paquete del
     **workspace** (fuente = commit), no de npm, y los tests Go siguen
     comparando campo a campo. La dirección no cambia: **zod → descriptor →
     Go**. Los tipos TypeScript no se generan desde el descriptor; son
     `z.infer` de la fuente. El descriptor no es un lenguaje de esquemas
     completo (no expresa refinamientos ni uniones discriminadas con reglas
     cruzadas), por eso es un artefacto derivado y no la fuente.
   - Se genera también `openapi.json` (OpenAPI 3.1 vía `z.toJSONSchema`) para
     clientes en otros lenguajes; CI regenera y falla ante cualquier diff,
     como hoy con el descriptor.
   - El contract set se reinicia como **4.0.0** (cambio de sobre a concesión).

9. **El SDK es `@kaana.ai/sdk`, en este repo (`packages/sdk`).** npm: el scope
   `@kaana.ai` es la organización del dueño; `npm view @kaana.ai/sdk` y
   `npm view @kaana.ai/contracts` dan 404 (2026-10-15) y `npm access list
   packages @kaana.ai` no lista nada: los nombres están libres y no hay ningún
   contrato publicado aún en el scope. Mismo repo que el servidor porque un
   cambio de contrato toca zod, Go y SDK en un solo PR con un solo gate. Bun
   workspaces (`packages/*`, `tools/contract`); se publica con
   `bun pm pack` + `npm publish <tarball> --provenance` desde un workflow por
   tag, como en Oxy. Sin dependencias de Oxy: el SDK no importa
   `@oxy.so/core`; la credencial es una función.

10. **Corte limpio.** Ningún paquete publica a la vez `@oxy.so/core/inference`
    y `@kaana.ai/sdk` como caminos válidos; ningún consumidor lleva flag,
    fallback ni doble cliente; `api.oxy.so/v1/{responses,chat/completions,
    audio/speech,images/generations,decisions,generations,models,realtime}` y
    `/models` se borran, no se redirigen. El alias `api.kaana.ai` se retira en
    el mismo corte.

## Arquitectura de la petición

```text
cliente (SDK @kaana.ai/sdk, SDK de OpenAI, curl)
  │  HTTPS / WSS  Authorization: Bearer <oxy_sk_… | service token | sesión>
  ▼
Cloudflare (proxy de kaana.ai; rate limit por IP en /v1/* y /internal/*)
  ▼
ALB dedicado de Kaana → servicio ECS `kaana` (≥ 2 tareas)
  │ 1. límite de cuerpo, parseo y validación con @kaana.ai/contracts
  │ 2. hechos sin contenido + sha256 del cuerpo
  │──── POST api.oxy.so/internal/kaana/v1/authorizations ───────────▶ Oxy
  │        (Kaana como workload, ADR 0026)                         │ autentica la credencial
  │                                                                │ política de la app, límites
  │                                                                │ reserve() en el ledger
  │◀──────────── concesión firmada Ed25519 (oxy-kaana-grant:v1) ────┘
  │ 3. verifica la concesión y el sha256
  │ 4. resuelve nivel/modelo → deployments → pool de claves
  │    (free → credits → paid; ADR 0001), reintenta y hace failover antes de la salida
  ▼
proveedor (OpenAI, Anthropic, Groq, ElevenLabs, …)
  │ stream → Kaana normaliza → SSE / audio chunked / WebSocket al cliente
  ▼
Kaana: usage report + provider_cost_events + fila de bandeja (una transacción)
  │──── POST api.oxy.so/internal/kaana/v1/settlements (firmado por Kaana) ──▶ Oxy settle()
  │      reintento con backoff; idempotente por reservationId
  └──── GET kaana.ai/internal/v1/settlements?cursor= ◀── reconciliación de Oxy
```

Dónde ocurre cada cosa:

| Paso | Dónde | Notas |
|---|---|---|
| TLS, rate limit por IP | Cloudflare | reglas nuevas para `/v1/*` |
| Límite de cuerpo antes de autenticar | Kaana | hoy lee hasta 16 MB antes de verificar (`app-kaana.tf:483-489`) |
| Autenticación de la credencial | Oxy (`authorizations`) | Kaana nunca guarda ni interpreta `oxy_sk_` |
| Política de app, límites, scopes | Oxy | igual que `admitRequest` hoy |
| Reserva | Oxy (`reserve`) | mismo dimensionado, con hechos que calcula Kaana |
| Elección de modelo, deployment y clave | Kaana | ADR 0001; `auto` por contenido solo puede vivir aquí |
| Ejecución, streaming, cancelación | Kaana | |
| Medición | Kaana | unidades, nunca importes |
| Liquidación y factura | Oxy (`settle`) | desde el recibo de Kaana |
| Recibo al cliente | Kaana sirve, Oxy pone el importe | |

Latencia: un salto Kaana→Oxy antes del primer token, en lugar del salto
cliente→Oxy→Kaana de hoy. Ambos están en `us-west-2`; Oxy ya hace hoy la misma
autenticación y reserva por petición.

## Superficie del SDK (`@kaana.ai/sdk` 1.0.0)

```ts
import type * as C from '@kaana.ai/contracts';

/** Valor de `Authorization: Bearer`. La función se llama en cada petición;
 *  el SDK no cachea: quien la da (OxyServices, OxyServer) ya cachea y renueva. */
export type KaanaCredential =
  | string
  | (() => string | Promise<string>);

export interface KaanaOptions {
  credential: KaanaCredential;
  /** Solo `https://kaana.ai` o loopback en desarrollo. */
  baseURL?: string;
  fetch?: typeof globalThis.fetch;
  /** WebSocket para realtime fuera de navegador/Bun (p. ej. `ws` en Node). */
  WebSocket?: typeof globalThis.WebSocket;
  timeoutMs?: number;          // por defecto 120_000
  maxRetries?: number;         // por defecto 2; solo antes del primer byte y códigos reintentables
}

export interface RequestOptions {
  signal?: AbortSignal;
  idempotencyKey?: string;     // Idempotency-Key
  /** Atribución delegada; solo válida con un service token. X-Oxy-User-Id. */
  onBehalfOfUserId?: string;
  timeoutMs?: number;
}

export class Kaana {
  constructor(options: KaanaOptions);

  readonly models: {
    list(options?: RequestOptions): Promise<C.Model[]>;
    get(id: C.ModelReference, options?: RequestOptions): Promise<C.Model>;
  };
  readonly powerLevels: {
    list(options?: RequestOptions): Promise<C.PowerLevel[]>;
  };
  readonly responses: {
    create(request: C.ResponsesRequest, options?: RequestOptions): Promise<C.Response>;
    stream(request: C.ResponsesRequest, options?: RequestOptions): KaanaStream<C.ResponseStreamEvent, C.Response>;
  };
  readonly chat: {
    completions: {
      create(request: C.ChatCompletionsRequest, options?: RequestOptions): Promise<C.ChatCompletion>;
      stream(request: C.ChatCompletionsRequest, options?: RequestOptions): KaanaStream<C.ChatCompletionChunk, C.ChatCompletion>;
    };
  };
  readonly embeddings: {
    create(request: C.EmbeddingsRequest, options?: RequestOptions): Promise<C.EmbeddingsResponse>;
  };
  readonly audio: {
    speech(request: C.SpeechRequest, options?: RequestOptions): Promise<SpeechResult>;
    speechStream(request: C.SpeechRequest, options?: RequestOptions): SpeechStream;
    transcriptions: {
      create(request: TranscriptionRequest, options?: RequestOptions): Promise<C.Transcription>;
    };
  };
  readonly images: {
    generate(request: C.ImageGenerationsRequest, options?: RequestOptions): Promise<C.ImageGenerations>;
  };
  readonly decisions: {
    create(request: C.DecisionRequest, options?: RequestOptions): Promise<C.DecisionSuccess>;
  };
  readonly generations: {
    get(id: string, options?: RequestOptions): Promise<C.GenerationRecord>;
    getByIdempotencyKey(key: string, options?: RequestOptions): Promise<C.GenerationRecord>;
  };
  readonly realtime: {
    connect(request: C.RealtimeSessionOpen, options?: RequestOptions): Promise<RealtimeSession>;
  };
}

/** Async-iterable de eventos validados; `final` resuelve con la respuesta
 *  plegada o rechaza con el KaanaError del evento terminal `error`. */
export interface KaanaStream<E, F> extends AsyncIterable<E> {
  readonly requestId: Promise<string>;
  readonly final: Promise<F>;
  abort(reason?: unknown): void;
}

export interface SpeechResult {
  audio: Uint8Array;
  mediaType: C.AudioMediaType;      // 'audio/mpeg' | 'audio/wav' | 'audio/ogg' | 'audio/aac' | 'audio/flac' | 'audio/pcm'
  model: C.ModelReference;          // lo que realmente sirvió
  requestId: string;
  usage: C.Usage;
}

/** Audio en streaming: trozos en orden, en cuanto el proveedor los produce.
 *  Por debajo usa `stream_format: 'sse'` para que un fallo a mitad llegue tipado. */
export interface SpeechStream extends AsyncIterable<Uint8Array> {
  readonly mediaType: Promise<C.AudioMediaType>;
  readonly requestId: Promise<string>;
  /** Para <audio>/MediaSource o para canalizar a un fichero o socket. */
  toReadableStream(): ReadableStream<Uint8Array>;
  /** Resuelve con modelo, uso y requestId al terminar; rechaza con KaanaError. */
  readonly done: Promise<Omit<SpeechResult, 'audio'>>;
  abort(reason?: unknown): void;
}

export interface TranscriptionRequest {
  model: C.ModelReference;
  file: Blob | Uint8Array;
  mediaType: C.AudioInputMediaType;
  language?: string;               // BCP 47
  prompt?: string;
  timestamps?: 'none' | 'segment' | 'word';
}

export interface RealtimeSession extends AsyncIterable<C.RealtimeServerEvent> {
  readonly sessionId: string;
  send(event: C.RealtimeClientEvent): void;
  appendAudio(pcm16: Uint8Array): void;
  resume(): Promise<void>;          // dentro de la ventana de 30 s (docs/realtime.md)
  close(): Promise<C.Usage>;       // la sesión liquida exactamente una vez
}

/* Errores tipados. `code` es el vocabulario público de @kaana.ai/contracts
 * (hoy INFERENCE_ERROR_CODES, contracts/src/inference/errors.ts:49-77). */
export class KaanaError extends Error {
  readonly code: C.ErrorCode;
  readonly status: number | null;   // null si llegó como evento terminal del stream
  readonly requestId: string | null;
  readonly retryable: boolean;      // de NON_RETRYABLE_INFERENCE_ERROR_CODES
  readonly retryAfterMs?: number;
  readonly param?: string;
  /** true si hubo salida antes del fallo (stream iniciado). */
  readonly afterOutput: boolean;
}
export class KaanaAuthenticationError extends KaanaError {}   // authentication_failed
export class KaanaPermissionError extends KaanaError {}       // permission_denied, insufficient_scope, commercial_permission_denied, policy_violation
export class KaanaInvalidRequestError extends KaanaError {}   // invalid_request, model_not_found, unsupported_modality, context_length_exceeded, request_too_large, output_limit_exceeded, idempotency_conflict
export class KaanaBillingError extends KaanaError {}          // insufficient_balance, spending_limit_exceeded, quota_exceeded
export class KaanaRateLimitError extends KaanaError {}        // rate_limited (retryAfterMs)
export class KaanaUnavailableError extends KaanaError {}      // no_route_available, deployment_unavailable, provider_*, service_unavailable
export class KaanaContentFilteredError extends KaanaError {}  // upstream_content_filtered
export class KaanaCancelledError extends KaanaError {}        // cancelled (abort del llamador)
export class KaanaInternalError extends KaanaError {}         // internal_error
/** Respuesta que no cumple @kaana.ai/contracts; nunca reintentable. */
export class KaanaProtocolError extends Error { readonly requestId: string | null; }
/** Fallo de red o timeout antes de cualquier respuesta. */
export class KaanaConnectionError extends Error { readonly cause: unknown; readonly timedOut: boolean; }
```

Cómo se conecta cada credencial (sin helpers en el SDK; el que tiene la
identidad da la función):

```ts
new Kaana({ credential: process.env.KAANA_API_KEY! });                 // oxy_sk_ de Console
new Kaana({ credential: () => oxyServer.serviceToken() });             // servicio Oxy (ADR 0026)
new Kaana({ credential: async () => (await oxy.getAccessToken())! });  // sesión de app Oxy
```

`byok_credential_invalid`, `provider_credential_invalid` y
`provider_billing_refused` se conservan en el vocabulario por compatibilidad
del contrato, pero al cliente le llegan como `KaanaUnavailableError` salvo
`byok_credential_invalid`, que es del cliente. Cambiar el vocabulario es una
pregunta abierta (ver abajo).

## Plan de corte por fases

Cada fase nombra repos, orden de merge y publicación. «Desplegado» significa
leído de vuelta en producción, no mergeado (`~/Oxy/docs/kaana-inference.md`,
«Source state is not production state»).

**Fase 0 — Decisión y documentación (sin código).**
1. Este ADR aceptado por el dueño con las preguntas abiertas resueltas.
2. Oxy ADR 0039 y Alia ADR 0016 (counterparts). Notas de «superseded» al
   principio de los ADR sustituidos, sin editar su cuerpo (convención de los
   índices de ADR de Oxy y Alia).

**Fase 1 — Contrato en Kaana** (repo Kaana).
1. `packages/contracts` (`@kaana.ai/contracts`): se trae
   `OxyHQServices/packages/contracts/src/inference/*` (sin `inbox.ts`,
   facturación ni `aliaModelRelease`) y `packages/api/src/schemas/inferenceEdge.schemas.ts`;
   se añaden concesión, liquidación, speech streaming, transcripción, voces,
   idiomas y formatos. Contract set 4.0.0.
2. `tools/contract` genera el descriptor desde el workspace; Go
   `internal/contract` adopta las shapes nuevas; `openapi.json` generado.
3. Biome deja de cubrir solo `tools/contract/**`: pasa a `packages/**` y
   `tools/**` (cambio explícito de la regla de `AGENTS.md`).
4. Publicar `@kaana.ai/contracts@4.0.0` (necesario para que Oxy lo consuma
   desde npm).

**Fase 2 — API interna en Oxy** (repo OxyHQServices). Depende de 1.4.
1. `POST /internal/kaana/v1/authorizations` y `/settlements`, reutilizando
   `authenticateEdgeCaller`, `admitRequest`, la política, el dimensionado,
   `reserve` y `settle` del borde actual, sin el reenvío. Concesión
   `oxy-kaana-grant:v1` con la clave de firma actual.
2. Service tokens con audiencia `kaana.ai`; resolución de sesiones de usuario
   con `clientId` y scope `inference:invoke` consentido.
3. Lista de precios firmada para el catálogo; verificación de la firma de
   liquidación de Kaana; reconciliación por cursor.
4. Desplegado en producción sin llamador (las rutas nuevas no abren nada
   por sí solas). El borde viejo sigue sirviendo hasta la fase 5.

**Fase 3 — Servidor público y SDK en Kaana** (repos Kaana y oxy-infra).
Depende de 2.4.
1. `internal/publicapi` (o ampliación de `internal/httpapi`): rutas `/v1/*`,
   cliente de autorización, verificación de concesión, bandeja de
   liquidación (migración nueva en `internal/credentialstore/migrations`),
   CORS, límite de cuerpo previo a la autorización, rate limit por credencial.
2. Adaptadores: transcripción expuesta, voces/idiomas/formatos en el
   publisher; speech streaming real por proveedor cuando exista.
3. `packages/sdk` (`@kaana.ai/sdk@1.0.0`) con tests contra un servidor de
   prueba que usa el mismo contrato; publicar.
4. oxy-infra: `desired_count ≥ 2` y autoescalado del servicio `kaana`; reglas
   Cloudflare de rate limit para `/v1/*` (requiere plan que lo permita);
   revisar que el TTFB de 125 s y el `idle_timeout` de 900 s del ALB cubren
   speech streaming y realtime.
5. Canario firmado de extremo a extremo con una clave de Console de prueba,
   leído de vuelta: concesión → ejecución → liquidación → recibo.

**Fase 4 — Consumidores** (en paralelo tras 3.5; cada uno quita
`@oxy.so/core/inference` y añade `@kaana.ai/sdk` en el mismo commit, con
`bun.lock`). Orden de merge recomendado, del riesgo menor al mayor:
1. **Homiio** (un `respond`).
2. **OxyHQServices** consumidores internos: `inboxInference.service.ts`
   (deja de llamar en proceso a `executeInferenceRequest` y pasa a
   `@kaana.ai/sdk` con el workload de Oxy y `onBehalfOfUserId`), Console
   playground (`packages/console/src/hooks/use-playground.ts` → SDK contra
   `kaana.ai/v1`), smoke `.github/scripts/decisions-release-smoke`, canario
   `packages/api/scripts/run-kaana-signed-canary.mjs`.
3. **Mention** (`respond`, `decide`, `getGenerationRecordByIdempotencyKey`;
   aprobaciones Jev revisadas) y su gate `scripts/validate-inference-boundary.mjs`.
4. **Alia** (`respond`, `stream`, `speech`, `listModels`, errores tipados en
   el turno). Un push a `main` de Alia despliega con migraciones: correr
   `test:pg` en local antes.
   Los gates de frontera (Alia `oxy-inference.ts` `OXY_INFERENCE_ALLOWED_ORIGINS`,
   Mention, Syra `scripts/validate-ai-architecture.mjs:67`, Nilo
   `apps/api/src/db/__tests__/inferenceBoundary.test.ts:404-406`) se invierten
   en el mismo cambio: `https://kaana.ai` está permitido **solo** a través de
   `@kaana.ai/sdk`; `/internal/` y los SDK de proveedores siguen prohibidos.

**Fase 5 — Retirada** (mismo día que el último despliegue de la fase 4, leído
de vuelta).
1. OxyHQServices: borrar `packages/core/src/inference` y el subpath
   `./inference` → **`@oxy.so/core@5.0.0`**; borrar
   `packages/contracts/src/inference/` (mudando `inbox.ts` y facturación fuera
   del directorio) → **`@oxy.so/contracts@5.0.0`**; borrar
   `routes/inferenceEdge.ts`, `inferenceRealtime.ts`, `/v1/models` y `/models`
   públicos, `schemas/inferenceEdge.schemas.ts`, `httpKaanaClient.ts` (parte
   de ejecución), `kaanaRealtimeClient.ts`, `INFERENCE_MACHINE_CREDENTIAL_AUTH`
   y `INFERENCE_EDGE_AUDIENCE`; `packages/api/openapi.json` sin `/v1` de
   inferencia.
2. Kaana: borrar `POST /internal/v1/{inference,decisions}`,
   `GET /internal/v1/realtime`, los sobres v1–v4 y el dominio
   `oxy-kaana-envelope:v1`; regla Cloudflare de `/internal/v1/inference`.
3. oxy-infra: retirar `api.kaana.ai` (cert SNI y host-header de
   `app-services.tf:16-21`, `:763-797`) y su DNS.
4. Docs del ecosistema (lista abajo). Sitio web: regenerar los docs
   sincronizados (`website/src/content/_synced/index.json` aún indexa
   `OxyInferenceClient`).

Publicación: `@kaana.ai/contracts@4.0.0` (1.4) → `@kaana.ai/sdk@1.0.0` (3.3) →
consumidores (fase 4) → `@oxy.so/core@5.0.0` y `@oxy.so/contracts@5.0.0`
(5.1). Ningún consumidor adopta `@oxy.so/core@5` antes de haber quitado su
import de `./inference`.

## Consumidores de `@oxy.so/core/inference` en `origin/main` (2026-10-15)

`git grep` de `OxyInferenceClient|createInferenceClient|@oxy.so/core/inference`
sobre la rama por defecto de los 35 repos de `~/Oxy` (excluidos registros de
auditoría `docs/audits/**`, que solo contienen hashes del paquete).

| Repo | Ficheros (no test) | Usa |
|---|---|---|
| Alia | `packages/api/src/lib/inference/oxy-inference.ts:10,93-138` (construye el cliente; origen permitido solo `https://api.oxy.so`, `:20`); `oxy-inference-credential.ts:45` (`OxyInferenceCredential`, service token por workload); `kaana-language-model.ts:43-44,610,651` (`respond`, `stream`, `OxyInferenceError`, `OxyInferenceProtocolError`, tipos `OxyResponsesRequest`/`OxyInferenceResponse`); `kaana-text.ts:86,100` (`respond`); `lib/models/catalogue.ts:277-280` (`listModels`); `lib/synthesize-speech.ts:28-34` (`speech` con `delegatedUserId`, MP3); `lib/errors/failover-error.ts:18`, `lib/inference/request-budget.ts:72`, `tool-limit.ts:35`, `routes/suggestions.ts:3`, `routes/v1/audio.ts:2` (`OxyInferenceError`); `buildOxyInferenceClientForServiceToken` para turnos de agentes de producto | `respond`, `stream`, `speech`, `listModels`, `delegatedUserId`, errores |
| Mention | `packages/backend/src/utils/oxyInference.ts:2-5,27-30,86` (`respond`, service token); `services/contentClassification/jevProduction.ts:4,57,111` (dos clientes); `jevSdk.ts:4,27-28,51,68` (`decide`, `getGenerationRecordByIdempotencyKey`); `jevReceipt.ts:16,130,140`; `controllers/posts/translation.ts:9` (`OxyInferenceError`); gate `scripts/validate-inference-boundary.mjs:151` | `respond`, `decide`, `getGenerationRecordByIdempotencyKey`, errores |
| Homiio | `packages/backend/services/oxyInferenceService.ts:2-6,72,109-113` (`respond`, service token); `routes/ai.ts:26` (`OxyInferenceError`) | `respond`, errores |
| OxyHQServices | el propio paquete `packages/core/src/inference/*`; fixture `packages/api/src/routes/__fixtures__/mentionWire.fixture.ts:287`; smoke `.github/scripts/decisions-release-smoke/smoke.{cjs,mjs}`; además, sin el cliente pero contra el borde: `services/inboxInference.service.ts:38,242,250` (en proceso) y `packages/console/src/hooks/use-playground.ts:150-156,246-253` (`fetch` a `api.oxy.so/v1/responses` y `/v1/generations/:id`) | todo |
| website | solo docs sincronizados (`src/content/_synced/index.json:30532-30534`, `:33038-33040`) | — |

Sin uso del cliente: Allo, Atlas, Clarity (vía Alia), Nilo/CRM (mismo repo;
solo gates y docs), CrowdSource, GoWay, Inbox (frontend llama a
`api.oxy.so/email/ai/*`, producto de Oxy), Mercaria (vía agente de Alia),
Moovo, Move, Noted, Peable, Schedio, Syra (gate y docs; «no unused inference
client is constructed», `docs/ai-routing.md:18-20`), tnp, Willo y el resto.

Tipos de inferencia importados de `@oxy.so/contracts` que pasan a
`@kaana.ai/contracts`: Alia (`InferenceMessage`, `InferenceStreamEvent`,
`InferenceErrorCode`, `InferenceContentPart`), Mention (`DecisionRequest`,
`DecisionSuccess`, `InferenceMessage`, `InferenceRequestOutcome`,
`inferenceEnvironmentSchema`, `UsageSource`, `UsageQuantity`), Homiio
(`InferenceMessage`, `InferenceContentPart`). Inbox importa
`inboxInference*` de `@oxy.so/contracts`, que se quedan en Oxy.

## Qué queda sustituido

- **Oxy ADR 0006** (`docs/adr/0006-oxy-kaana-boundary.md:60`, `:123-130`):
  el borde público y la experiencia de desarrollador pasan a Kaana; Kaana
  sigue sin autorizar clientes porque la autorización la decide Oxy en
  `authorizations`.
- **Oxy ADR 0005** (`docs/adr/0005-oxy-is-the-single-control-plane.md`) se
  mantiene: Oxy sigue siendo el único plano de control; lo que cambia es que
  el plano de datos recibe la petición y pregunta, en vez de recibirla
  reenviada.
- **Oxy ADR 0015** (`docs/adr/0015-oxy-kaana-envelope-signing.md`): el
  mecanismo de firma se conserva; lo firmado pasa a ser la concesión.
- **Oxy ADR 0038, fase 2**: pasa de «planificada» a prerrequisito de
  cualquier `auto` que dependa del contenido (Oxy ya no ve el prompt).
- **Alia ADR 0001** y **ADR 0010** (frase «generic inference is Kaana through
  Oxy»); ADR 0012 y 0014 citan `OxyInferenceClient.listModels()`.
- `~/Oxy/AGENTS.md:19-21` (sin cambio de fondo; añadir «API pública
  `kaana.ai/v1`») y `~/Oxy/docs/kaana-inference.md` (diagrama
  `app -> Oxy inference edge -> Kaana`, «Oxy decides … signs an ordered list
  of opaque deploymentIds»).
- `~/Oxy/docs/project-map.md:9-12` y `~/Oxy/docs/alia-agency-capability-architecture.md`.
- Alia `AGENTS.md:36-38` («`Alia -> Oxy -> Kaana` con `OxyInferenceClient`»),
  `README.md:90`, `docs/chat-runtime.mdx:35`, `docs/model-abstraction.mdx:90,198,231`,
  `docs/deployment.md:170`, `docs/api-reference.md:300`, `docs/onboarding.md:133`,
  `docs/runbooks/kaana-cutover.md`, `docs/runbooks/rollback.md:5`,
  `packages/api/.env.example:78`.
- OxyHQServices `AGENTS.md:3-5,70`, `docs/inference/{README,sdk,request-routing,realtime,streaming,alia,migration}.md`,
  `docs/architecture/inference-responsibility-matrix.md`.
- Kaana `AGENTS.md:4-6`, `README.md:20,138,187`, `docs/architecture.md:7-17`,
  `docs/identity-and-routing.md:33-40`, `docs/contract.md:3-13`,
  `docs/decisions.md:3-10`, `docs/realtime.md`, `docs/operating.md:907-916`
  (además ya desfasado: cita una zona privada de Route 53 que oxy-infra
  eliminó).
- Clarity `AGENTS.md:6,16`, README; Nilo `AGENTS.md:22-25`, README,
  CONTRIBUTING; Noted `AGENTS.md:58-63`, README, `docs/index.mdx:16-17`;
  Syra `docs/ai-routing.md:3-16` y `.github/workflows/quality.yml:143-145`;
  Mention `docs/jev-shadow-evaluation.md:30`; Mercaria
  `packages/backend/.env.example:419-420`; Inbox README.

## Consecuencias

- Kaana pasa a ser un servicio expuesto a clientes y a terceros: su
  disponibilidad es ahora la de toda la IA de Oxy sin un borde delante que
  absorba picos. Un `desired_count = 1` deja de ser aceptable.
- El prompt y la salida dejan de cruzar Oxy: Oxy ADR 0016 se cumple por
  construcción, y Oxy ya no puede inspeccionar contenido para su política.
- Oxy conserva toda la autoridad económica: sin concesión no hay ejecución, y
  la liquidación sigue saliendo del recibo medido por Kaana.
- Kaana gana una clave privada (firma de liquidaciones) y una tabla de
  bandeja de salida; hasta hoy solo guardaba claves públicas.
- Un fallo de Oxy (`authorizations`) bloquea toda la inferencia, como hoy un
  fallo del borde. No hay modo degradado que ejecute sin concesión.
- El SDK de OpenAI y cualquier cliente compatible apuntan a
  `https://kaana.ai/v1`; `api.oxy.so/v1` deja de responder a inferencia.

## Riesgos

1. **Terceros en `api.oxy.so/v1`.** `docs/inference/sdk.md:181-193`,
   `:260-280` documenta el SDK de OpenAI con `oxy_sk_` contra
   `api.oxy.so/v1`. El corte sin redirección rompe a cualquier tercero que lo
   use. Hay que medir el tráfico `oxy_sk_` real antes (la vía está cerrada
   por defecto con `INFERENCE_MACHINE_CREDENTIAL_AUTH`, `inferenceEdge.service.ts:430-435`).
2. **Ventana del corte.** Entre la fase 3 y la 5 existen en producción dos
   caminos aunque ningún cliente use los dos. El borde viejo debe borrarse en
   el mismo día; si se alarga, contradice la decisión del dueño.
3. **Private Auto y Jev.** Hoy `bindPrivateAutoExecution` recibe
   `request.input` en Oxy (`inferenceEdge.service.ts:1940-1948`) y las
   aprobaciones Jev de Mention fijan `deploymentId`, proveedor y
   `priceVersionId` revisados. Con la concesión, Oxy ve solo hechos y el
   `sha256`; esas aprobaciones congeladas deben reemitirse y revisarse.
4. **Escala y abuso.** Kaana lee hasta 16 MB antes de verificar una firma
   (`app-kaana.tf:483-489`); con un endpoint público hace falta límite de
   cuerpo por ruta y rate limit en Cloudflare, que exige plan Business o
   superior para reglas por path (runbook 38 de oxy-infra).
5. **Secretos de CI.** Los secretos de organización no llegan a repos privados
   en GitHub Free (por eso el DNS de Kaana vive en un workflow de
   OxyHQServices, `cloudflare-dns-kaana.yml:3-7`). Publicar en npm desde el
   repo privado de Kaana necesita un secreto de repositorio o trusted
   publishing.
6. **Latencia de autorización.** `authorizations` es síncrona y sin caché
   (como ADR 0035 exige para `oxy_sk_`). Un p99 alto de Oxy se suma al TTFB.
7. **Sesiones de usuario en el navegador.** Abrir CORS y aceptar sesiones
   pone la inferencia al alcance de cualquier app registrada; la política de
   la app y los límites por usuario deben estar listos antes.
8. **Concurrencia de sesiones.** Hay una rama `feat/speech-stream` en un
   worktree de este repo creada por otra sesión; el diseño de speech streaming
   debe coordinarse con ella antes de implementar.
9. **Bundle de Expo.** Si el SDK valida con zod en tiempo de ejecución, una app
   React Native que lo use con sesión de usuario carga zod y todo el
   contrato.

## Preguntas abiertas para el dueño

1. **¿Quién paga una petición con sesión de usuario?** Propuesta: la
   aplicación del `clientId` de la sesión, con límites por usuario. Otra
   opción: el saldo del propio usuario.
2. **¿Fase 2 de ADR 0001 (Kaana resuelve niveles) entra en el corte o
   después?** Propuesta: después; la concesión lleva las `authorizedRoutes`
   que Oxy firma hoy y el `auto` sigue siendo el de política sin contenido
   hasta la fase 2.
3. **¿Se mide el tráfico de terceros en `api.oxy.so/v1` y se avisa antes de
   cortar, o se corta sin aviso?** La decisión dice «sin deprecación»; esto
   pregunta solo por la comunicación.
4. **¿Precios visibles en `kaana.ai/v1/models`?** Propuesta: sí, la lista de
   precios firmada por Oxy. Otra opción: el catálogo de Kaana sin precios y
   los precios solo en Console.
5. **¿Repo del SDK?** Propuesta: este repo (`packages/sdk`). Alternativa: un
   repo público `OxyHQ/kaana-sdk` (también facilitaría los secretos de CI y
   aceptar contribuciones), con el contrato publicado desde Kaana.
6. **¿Vocabulario de errores público?** ¿Se exponen
   `provider_credential_invalid` y `provider_billing_refused` a terceros o se
   pliegan en `provider_error`?
7. **¿Audiencia del service token?** Propuesta: `aud: kaana.ai` dedicada;
   alternativa: aceptar `oxy-api` en Kaana (más simple, más amplio).
8. **¿Se conserva `images/generations` y `decisions` en el primer corte?**
   Ambos existen hoy en Oxy; decisions está dormida.
