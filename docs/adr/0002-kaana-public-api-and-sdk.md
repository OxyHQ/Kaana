# ADR 0002 — Kaana sirve su API pública `https://kaana.ai/v1`, resuelve los niveles de potencia y publica su propio SDK; Oxy Console gestiona, Oxy autoriza, reserva y liquida

- Status: accepted (el dueño lo aceptó el 2026-10-15; sus respuestas al
  primer borrador están incorporadas como decisiones)
- Date: 2026-10-15
- Decided by: el dueño de Oxy (decisión de 2026-10-15 y sus respuestas del
  mismo día; este documento las desarrolla, no las reabre)
- Supersedes, al aceptarse: Oxy ADR 0006 (fila «Public API edge and developer
  experience | Oxy» y «Kaana no autoriza clientes»), Oxy ADR 0015 en lo que
  firma (el sobre deja de llevar la petición y pasa a ser una concesión), Oxy
  ADR 0038 fase 2 (deja de estar planificada: se ejecuta en este corte), Kaana
  ADR 0001 decisión 4 y fase 2 (ídem), Alia ADR 0001 («Alia neither signs nor
  calls Kaana directly», «uses the published `OxyInferenceClient`») y la frase
  de Alia ADR 0010 «generic inference is Kaana through Oxy». Ver «Qué queda
  sustituido».
- Counterparts por escribir (fase 0): Oxy ADR 0039 (API interna de
  autorización y liquidación; Console como única superficie de gestión;
  retirada de `@oxy.so/core/inference` y del borde `api.oxy.so/v1`) y Alia
  ADR 0016 (Alia consume `@kaana.ai/sdk`).
- Compatible con: Kaana ADR 0001 decisiones 1–3 / Oxy ADR 0038 fase 1, Oxy
  ADR 0005 (Oxy es el único plano de control), 0007 (atribución canónica),
  0009 (reserva y liquidación), 0011 (origen único `https://kaana.ai`), 0016
  (sin persistencia de payloads), 0026 (servicios propios como workloads),
  0035 (introspección `oxy_sk_` por un servidor de recursos), 0037 (Pro/Ultra
  de Alia solo con crédito promocional).

## Contexto

La decisión del dueño (2026-10-15): Kaana es un proveedor de inferencia como
OpenRouter u OpenAI, para terceros y para Oxy/Alia. Su API pública es
`https://kaana.ai/v1` y tiene su propio SDK de TypeScript. Oxy se queda con
identidad, Oxy Console, las claves `oxy_sk_…`, facturación y límites; Kaana ya
elige proveedor y clave (2026-10-12). El corte es limpio:
`@oxy.so/core/inference` (`OxyInferenceClient`, `createInferenceClient`)
desaparece sin adaptador, sin deprecación y sin doble camino; todos sus
consumidores migran en el mismo corte.

Estado medido en `origin/main` de cada repo el 2026-10-15 (Kaana `c1132d1`,
OxyHQServices `c87e7f5da`, Alia `28541bbac`, oxy-infra `0a4ad6b`). Las rutas
sin prefijo de repo son de Kaana; `O:` es OxyHQServices, `A:` Alia, `I:`
oxy-infra.

### Kaana no tiene API pública

`internal/httpapi/server.go:3-5` («internal signed routes and no
customer-facing one»); rutas en `server.go:183-193`:
`POST /internal/v1/{inference,decisions}`, `GET /internal/v1/realtime`
(WebSocket), `GET /internal/v1/{health,models}`,
`POST /internal/v1/{models,deployments}/query`, validaciones de credenciales de
cliente, feeds de telemetría y `GET /livez` (la única sin firma). La
autenticación es un sobre Ed25519 de Oxy (`internal/edgeauth/edgeauth.go:13-35`,
cabeceras `X-Oxy-Kaana-{Key-Id,Timestamp,Signature}`, dominio
`oxy-kaana-envelope:v1`, ventana de 5 min sin nonce, `:87-94`); claves públicas
por env `KAANA_EDGE_PUBLIC_KEYS` (`cmd/kaana/main.go:78-90`). No hay JWT ni
JWKS en Kaana. `docs/architecture.md:7-17` y `:310-335` prohíben a Kaana
cuentas, credenciales de API de Oxy, ledger y consola;
`docs/identity-and-routing.md:33-40` dice que todo acceso de desarrollador
pasa por el borde de Oxy.

### El borde público de inferencia es Oxy, en `api.oxy.so/v1`

`O:packages/api/src/server.ts:842` (`app.use('/v1', inferenceEdgeRoutes)`),
`:846` y `:861` (catálogo), `:357-360` (WebSocket realtime). Rutas en
`O:packages/api/src/routes/inferenceEdge.ts`: `POST /v1/decisions` (`:879`),
`/v1/responses` (`:978`), `/v1/chat/completions` (`:1088`, dialecto OpenAI),
`/v1/audio/speech` (`:1234`), `/v1/images/generations` (`:1334`),
`GET /v1/generations/by-idempotency-key` (`:1418`), `/v1/generations/:id`
(`:1493`); `/v1/audio/transcriptions` no existe a propósito (`:1590-1646`).
Realtime: `O:routes/inferenceRealtime.ts:108-121`. Ciclo de vida en
`O:packages/api/src/services/inferenceEdge.service.ts`: autenticar
`authenticateEdgeCaller` (`:439-531`; `oxy_sk_` solo si
`INFERENCE_MACHINE_CREDENTIAL_AUTH`, `:430-435`; service token JWT
`aud: oxy-api`; **la sesión de usuario se rechaza**, `:474-477`), scopes
`inference:invoke` (`:1003-1008`), política (`:1063-1067`), preflight
`attestDeployments` (`:1885-1902`), dimensionado del hold (`:1951`), `reserve`
(`O:services/inferenceLedger.service.ts:744`, llamado en
`inferenceEdge.service.ts:2235`; TTL 900 s, `:298`), `buildEnvelope`
(`:3788`), `kaanaClient.execute`/`stream` (`:2373`, `:2736`) y `settle`
(`inferenceLedger.service.ts:951`). Oxy liquida con el `usage_report` que
Kaana envía en el mismo SSE (`server.go:58-61`, `:415-424`).

### Oxy Console ya es la superficie de gestión

Console (`O:packages/console`) crea aplicaciones y credenciales, gestiona la
política de inferencia por app (`O:packages/console/src/components/apps/routing-policy-{form,section}.tsx`,
`hooks/use-routing-policy.ts:69,148` → `PUT/GET /inference/routing-policies/applications/:id`,
`O:server.ts:870`), presupuestos (`routes/_layout/billing/budgets.tsx`,
`components/apps/application-budgets-section.tsx`), uso y facturación
(`hooks/use-inference-reporting.ts`), documentación para developers
(`routes/_layout/documentation/{quickstart,authentication,chat-completions,models,sdks}.tsx`,
todas apuntan a `https://api.oxy.so/v1`, p. ej. `sdks.tsx:78,145,172`,
`chat-completions.tsx:111,176`, `quickstart.tsx:170`, `authentication.tsx:167`)
y el playground (`routes/_layout/playground.tsx`,
`hooks/use-playground.ts:150-156,246-253`: `fetch` desde el navegador a
`api.oxy.so/v1/responses` y `/v1/generations/:id` con la `oxy_sk_` que pega el
usuario, guardada solo en memoria, `lib/playground-key.ts`).

### Quién paga hoy cada petición

Regla de fondo: **paga la cuenta dueña de la aplicación que se autenticó; el
usuario final nunca paga en Oxy** (`O:docs/inference/billing.md:106-110`,
`O:docs/inference/attribution.md:78-84`). `LedgerAttribution.accountId` es la
cuenta del workload y `userId` nunca entra en un saldo
(`inferenceLedger.service.ts:89-103`). `resolveBillingAccount` sube al ancestro
más profundo con perfil de facturación activo (`:173-222`). Las bolsas se
gastan en `RESERVATION_DRAW_ORDER` = crédito promocional → fondos comprados →
factura (`O:db/schema/billingLedgerEntries.ts:158-162`; `computeDraw`,
`inferenceLedger.service.ts:1672-1711`). `reserve` bloquea el saldo, comprueba
idempotencia, evalúa límites de gasto (`:671-683`, alcances cuenta-subárbol,
aplicación y credencial, `O:db/schema/spendingLimits.ts:73,82,87`), aplica
`fundingRestriction: 'promotional-only'` si toca (`:689-705`) y retiene. Los
créditos de plan o suscripción del usuario (Oxy ADR 0033, `user_credits` de
Alia) son de producto y no entran en el ledger de inferencia
(`O:services/entitlement.service.ts:17-24`; Alia ADR 0005).

| Carril / credencial hoy | Paga | Usuario atribuido | Tratamiento y fuentes | Techos y límites | Evidencia |
|---|---|---|---|---|---|
| `machine_credential` (`oxy_sk_` de Console) | dueño de la app → ancestro facturable | `X-Oxy-User-Id` o `user` de OpenAI, solo forma (≤ 64) | `commercial`: promo → comprado → factura | límites de gasto; rate limit por minuto por credencial y por app; `maxPricePerRequest` de la política | `inferenceEdge.service.ts:452-468`; `O:middleware/machineCredential.ts:347-361` |
| `oxy_sk_` de un tercero reenviado por Alia (ADR 0035) | dueño de la app **llamante** | ninguno | `commercial` | igual | `O:docs/adr/0035-alia-app-only-machine-chat.md`; `A:lib/chat/machine-turn.ts:25-44` |
| `service_token` genérico (ADR 0026) | `ownerAccountId` de la fila de la app | `X-Oxy-User-Id` opcional | `commercial` | límites de gasto | `inferenceEdge.service.ts:493-530` |
| `service_token` de Alia en producción (relación `alia-kaana`) | **nadie**: `internal_metered`, sin hold ni recibo de dinero | `delegatedUserId` | sin fondos Oxy (Alia cobra a su usuario en sus propios créditos) | capacidad 32 concurrentes / 5000 por día UTC; piloto: tuplas exactas, cotización ≤ USD 0,05; `pro`/`ultra` solo crédito promocional (≤ USD 1,00, `maxFundingTier: 'credit'`, si no hay, `xhigh`) | `O:config/inferenceEconomicPolicy.ts:138-197,227-244`; `inferenceEdge.service.ts:931-942,1050-1061,1292-1310,1561-1574,2017-2027,2270-2279`; `O:services/inferenceInternalPilot.ts:104-128`; Oxy ADR 0037 |
| `service_token` de Alia fuera de producción | dueño de la app Alia | delegado | `commercial` | límites de gasto | `inferenceEconomicPolicy.ts:144` |
| Agente de producto en Alia (Sindi/Homiio, Clarity): Alia reenvía el service token verificado del producto | dueño de la **app de producto** | `req.user.id` | `commercial` | límites de gasto | `A:packages/api/src/lib/inference/oxy-inference.ts:106-125`; `O:docs/inference/alia.md:105-113` |
| `product_session` (Inbox `/email/ai/*`, sesión humana + credencial de servicio de Inbox como ancla) | dueño de la app Inbox | usuario de la sesión | `commercial` (nunca interno) | límites de gasto; perfil fijo `power-instant` | `O:services/inboxInference.service.ts:109-121`; `O:config/inboxInference.ts:23` |
| Permiso scoped (cualquier carril) | dueño | — | solo `promotional_funds` | cotización ≤ `maxCostUsd` | `O:services/scopedExecution.service.ts:13-14`; `inferenceEdge.service.ts:1150-1155,2029,2236` |
| Clasificador de Mention, hijo de Private Auto | puede ser `internal_metered` | sin delegado | aprobación de fuente que fija deployment, proveedor, clave y `priceVersionId` | techos propios en USD | `inferenceEdge.service.ts:978-994,2032-2038`; `O:services/mentionClassifierEconomics.service.ts:89` |
| BYOK (`customerProviderCredential`) | el proveedor factura al cliente; Oxy cobra comisión al dueño | — | `platformFeeOnly` | `maxFundingTier` prohibido junto a BYOK | `inferenceEdge.service.ts:2510`; `O:packages/contracts/src/inference/routingPolicy.ts:411-416` |

**Techo de financiación y tier servido.** Kaana #169 (`5151eba`) ejecuta cada
deployment sobre el pool de claves de su proveedor, free → promocional →
prepago → pago → sin declarar (`internal/provider/funding.go:72-112`), y añade
`KeyPool.WithinFunding` (`internal/provider/credential.go:538-560`). Kaana #171
(`c1132d1`, contracts 4.12.0 / set 3.8.0) cablea
`authorizedRoutes[].maxFundingTier` (`free|credit|paid`,
`O:packages/contracts/src/inference/identifiers.ts:265-277`): el ejecutor acota
el pool y salta la ruta si no queda clave admisible
(`internal/kaana/executor.go:326,1232-1236,1387`; también realtime), e informa
`usage.fundingTier` (`executor.go:942`). En Oxy, `fundingTier` solo alimenta
`fundingCeilingBreach` (`inferenceEdge.service.ts:3727-3744,2459,2842`); **el
precio al cliente no depende del tier** (`settle` usa el `priceVersionId` de la
ruta servida, `:2496-2510`; Oxy ADR 0038:106-109).

Campos de pagador que hoy viajan firmados (`attributionFor`,
`inferenceEdge.service.ts:3897-3912`; `O:packages/contracts/src/inference/attribution.ts:70-117`):
`principal.billing.accountId` (`.strict()`, único pagador), `applicationId`,
`credentialId`, `environment`, `inferenceScopes`, `userId`, `requestId`,
`authorizedRoutes[].maxFundingTier`, `authorizedRoutes[].customerProviderCredential`,
`routingPolicy`, `idempotencyKey`, `scopedExecution` (v3), `privateAutoExecution`
(v4). **No viajan** (viven solo en la admisión o el ledger): `lane`,
tratamiento económico y relación, `fundingRestriction`, el hold y su
`reservationId`, los techos del piloto, la capacidad y los límites de gasto.

### Niveles de potencia hoy (los resuelve Oxy)

Pertenencia por **clase revisada** del modelo: tabla
`inference_model_power_classes` (`O:db/schema/inferenceModelPowerClasses.ts:32-67`),
sembrada por `O:drizzle/0129_power_routing_profiles.sql:14-30` con las bandas
del AA Intelligence Index v4.3.2 (`O:docs/inference/power-levels.md:185-192`);
solo cambia por migración revisada (`:264-268`). `POWER_LEVEL_CLASS` (`xhigh`
usa la clase `high`, `O:services/inferencePowerLevels.service.ts:72-79`),
`powerClassModelIds` (`:292-315`), `powerLevelCandidates`
(`O:services/inferenceCatalogue.service.ts:2960-2989`), orden por prioridad →
BYOK → clase de financiación → score → `deploymentId`
(`O:docs/inference/routing.md:352-361`; `fundingPriorityFor`,
`inferenceCatalogue.service.ts:2802-2860`, con `discounted_payg` **antes** que
`promotional_credit`), preferencia por no razonar en `instant`
(`power-levels.md:39-42`), regla de esfuerzo `resolvePowerLevelEffort`
(`inferencePowerLevels.service.ts:111-122,267-283`) con mismo esfuerzo en
failover (`power-levels.md:80-84`). `auto` es determinista
(`classifyAutoPowerLevel`, `:173-231`: 8k/64k tokens de entrada, 4096/16000
de salida, 8 herramientas; techo `xhigh`, `:82,239-245`); el clasificador Jev
está implementado y dormido (`O:config/autoClassification.ts:37-40`). La
política por app (`allowedRoutingProfileIds`, `defaultTarget`, topes de
precio, proveedores, regiones, ZDR, fallback, BYOK; `routing.md:93-108`) se
aplica en `inferenceEdge.service.ts:1505-1531`. Kaana no resuelve nada:
`powerLevelSchema` y `modelPowerClassSchema` están marcados «not exchanged»
(`internal/contract/descriptor_test.go:357-358`) y el target
`routing_profile_id` se ejecuta solo vía `authorizedRoutes`
(`docs/routing.md:55-62`).

### El cliente y el contrato

`O:packages/core/src/inference/` es `OxyInferenceClient.ts` (1042 líneas) e
`index.ts`: origen `https://api.oxy.so` (`:93`), credencial `string | () =>
string | null | Promise<string | null>` (`:103`), métodos `listModels`,
`getModel`, `listRoutingProfiles`, `respond`, `decide`, `speech`, `stream`,
`getGeneration`, `getGenerationRecord`, `getGenerationRecordByIdempotencyKey`
(`:399-722`), opciones `signal`, `idempotencyKey`, `delegatedUserId`
(`:180-199`), errores `OxyInferenceError` y `OxyInferenceProtocolError`
(`:283-330`). `createInferenceClient(oxy)` usa la sesión, que el borde rechaza.
El contrato es zod en `O:packages/contracts/src/inference/` (set `3.8.0`,
`version.ts:103`), mezclado con facturación, `inbox.ts` y `aliaModelRelease`;
las peticiones públicas viven en `O:packages/api/src/schemas/inferenceEdge.schemas.ts:113-672`.
Kaana genera `internal/contract/descriptor.json` desde el paquete publicado
(`tools/contract/generate.mjs:1-32`); no emite tipos TypeScript.

### Audio y voz

`audio_speech` es solo no-streaming (`internal/contract/request.go:438-439`);
Kaana trocea el archivo completo en eventos `audio` de ≤ 49 152 bytes
(`docs/elevenlabs.md:101-103`). La rama `feat/speech-stream` de este repo
(trabajo de esta misma iniciativa) hace que los adaptadores de síntesis
reenvíen el audio según llega **dentro del sobre actual**, sin cambiar el
contrato (`internal/provider/audio_relay.go` y los adaptadores ElevenLabs,
xAI y Deepgram, sin mergear a 2026-10-15). `speed`: el contrato admite
0,25–4 (`request.go:444`), xAI 0,7–1,5
(`internal/provider/openaicompat/speech.go:55-56`), ElevenLabs 0,7–1,2
(`docs/elevenlabs.md:83-86`), Deepgram lo rechaza
(`internal/provider/deepgram/adapter.go:90-91`), y Alia acepta 0,7–1,5
(`A:packages/api/src/routes/v1/audio.ts:45`). Hoy ese rechazo ocurre en el
`Translate` del adaptador, **después** de reservar. El inventario
(`internal/inventory/inventory.go:104-130`) no lleva voces, idiomas, formatos,
rango de `speed` ni máximo de caracteres (ElevenLabs los tiene solo en docs,
`docs/elevenlabs.md:31-35,68-86`).

### El host y la publicación

`kaana.ai` es un CNAME proxied por Cloudflare hacia un ALB público dedicado
(`I:terraform-uswest2/app-kaana.tf:380-401`) cuyo listener reenvía todo a
Kaana sin reglas de path (`:490-501`); SG solo Cloudflare (`:254-314`); Oxy
llama por el mismo camino (`I:app-services.tf:113`); TTFB de Cloudflare 125 s
(`app-kaana.tf:555-584`); `desired_count = 1` (`:961-1024`); `api.kaana.ai` es
un host-header de oxy-api (`I:app-services.tf:16-21`). Hoy `kaana.ai/v1/...`
da 404. El repo `OxyHQ/Kaana` es **público** (`gh repo view OxyHQ/Kaana`,
2026-10-15). Oxy publica sus paquetes con un workflow `workflow_dispatch` en
dos trabajos (build de bytes de confianza → publish que reconstruye y compara)
y el secreto de repositorio `NPM_TOKEN`
(`O:.github/workflows/release-decisions-packages.yml:3-17,103-134`;
`O:.github/scripts/release-decisions-packages.mjs:437`), siempre
`npm publish <tgz>` (`O:docs/engineering/package-rules.md:71-75`). En npm,
`@kaana.ai/sdk` y `@kaana.ai/contracts` dan 404 y el scope `@kaana.ai` no
lista paquetes (2026-10-15).

## Decisión

1. **Oxy Console es la única superficie de gestión.** Crear aplicaciones y
   `clientId`; crear, rotar y revocar `oxy_sk_…`; política de inferencia por
   app (modelos y niveles permitidos, objetivo por defecto, topes de precio,
   proveedores y regiones, ZDR, BYOK); límites y presupuestos; uso,
   facturación y recibos; el playground; y la documentación para developers.
   Kaana no tiene consola, registro de developers, cuentas ni claves propias:
   `kaana.ai/v1` solo ejecuta inferencia y acepta las credenciales que emite
   Oxy. Todo lo de cuentas y claves en Kaana (página raíz, docs, mensajes de
   `authentication_failed` y de saldo) enlaza a `https://console.oxy.so`.

2. **Kaana sirve `https://kaana.ai/v1` en su propio proceso Go.** No se mueve
   el borde de Oxy de host. Lo que decide quién puede gastar y cuánto se
   queda en Oxy como **API interna** que Kaana consulta; lo que toca la
   petición (parseo, validación, streaming, dialecto OpenAI, audio, realtime,
   resolución de niveles, elección de modelo, deployment y clave) es de
   Kaana. El payload deja de pasar por Oxy. Mover el borde de Oxy a
   `kaana.ai/v1` daría dos saltos y dos parsers de SSE por token, el
   WebSocket proxificado dos veces y el límite de 125 s de Cloudflare en ambos
   saltos, y obligaría a que el `auto` por contenido siguiera en Oxy.

3. **Oxy decide quién gasta: concesión y liquidación.** Cada petición
   facturable abre una concesión con
   `POST https://api.oxy.so/internal/kaana/v1/authorizations` y se cierra con
   `POST https://api.oxy.so/internal/kaana/v1/settlements`.
   - Kaana se autentica ante Oxy como workload (ADR 0026), sin par de claves.
   - Kaana manda la credencial del cliente **tal cual**, opaca, más hechos sin
     contenido: operación, objetivo pedido (`publisher/model[@revision]` o
     nivel), `estimatedInputTokens`, `maxOutputTokens`, caracteres o segundos
     de audio, herramientas declaradas, `stream`, `Idempotency-Key`,
     `X-Oxy-User-Id`, el `sha256` del cuerpo normalizado y, para las
     operaciones con aprobación de fuente (Jev, Private Auto), el hash
     canónico de entrada que esa aprobación fija. Nunca el prompt, el audio
     ni la salida.
   - Oxy resuelve el principal, aplica la política de Console, la capacidad y
     los rate limits por credencial y app, cotiza y **reserva** con el ledger
     actual, y devuelve una **concesión firmada** Ed25519 (dominio nuevo
     `oxy-kaana-grant:v1`, mismas claves `KAANA_EDGE_PUBLIC_KEYS`) que
     transporta la matriz de financiación completa (decisión 4).
   - Kaana verifica firma, caducidad (60 s para empezar a ejecutar) y
     `bodySha256`, y ejecuta dentro de la concesión. Sin concesión válida no
     hay llamada a ningún proveedor.
   - **Liquidación exactamente una vez.** Al terminar (éxito, error,
     cancelación o cierre de sesión realtime) Kaana escribe el
     `normalizedUsageReport` en una **bandeja de salida durable en su
     PostgreSQL**, en la misma transacción que sus `provider_cost_events`; un
     worker lo entrega firmado por Kaana (dominio `kaana-oxy-settlement:v1`,
     clave privada nueva de Kaana en SSM; Oxy guarda la pública), idempotente
     por `reservationId`. Oxy expira lo no liquidado con `expireReservations`
     (`inferenceLedger.service.ts:1247`), libera la plaza de capacidad, y
     reconcilia leyendo `GET kaana.ai/internal/v1/settlements?cursor=` como ya
     lee la telemetría de proveedor. Kaana sigue sin conocer importes de
     cliente salvo los que Oxy pone en la concesión para mostrarlos.

4. **La concesión transporta exactamente el modelo de financiación actual.**
   El corte no inventa política. Campos (en `@kaana.ai/contracts/internal`):

   | Campo | Contenido | Sustituye a |
   |---|---|---|
   | `principal` | `billingAccountId` (dueño de la app, sin resolver; el ancestro facturable lo resuelve el ledger), `applicationId`, `credentialId`, `environment`, `scopes`, `lane` (`machine_credential` · `service_token` · `product_session`) | `principal` firmado hoy + `lane` |
   | `userId` | delegado o de sesión; solo atribución | `userId` |
   | `economics` | `treatment` (`commercial` · `internal_metered` · `platform_fee_only`), `policyVersion`, `relationshipId` | `economics` de la admisión |
   | `reservation` | `reservationId`, `ceiling {amount, currency}` (ausente si `internal_metered`), `fundingRestriction` (`promotional-only` para permisos scoped) | el hold de `reserve` |
   | `capacity` | `meteredUsageId` de la plaza reclamada (32/5000 de `alia-kaana`) | `claimMeteredAdmission` |
   | `steps[]` | escalones en orden; cada uno `target` (`{kind:'model', modelLine}` o `{kind:'level', level}`) + `limits` (`maxPricePerRequest`, `maxFundingTier`, `admittedModelLines[] {modelLine, priceVersionId}`, proveedores, regiones, ZDR, no-training, licencias) | `authorizedRoutes[]` con `maxFundingTier` |
   | `exactRoute` | solo para operaciones con aprobación de fuente (scoped, Private Auto, clasificador de Mention): deployment, proveedor, clave y `priceVersionId` aprobados | sobres v3/v4 |
   | `customerProviderCredential` | handle BYOK; excluye `maxFundingTier` | igual que hoy |
   | `idempotencyKey`, `requestId`, `client.labels`, `routingPolicyVersionId` | igual que hoy | igual |

   Cómo queda cada carril:
   - `oxy_sk_` de Console → `machine_credential`, `commercial`, pagador el
     dueño de la app; rate limits por credencial y por app los aplica Oxy en
     `authorizations` (hoy `O:middleware/machineCredential.ts:347-361`).
   - `oxy_sk_` reenviado por Alia (ADR 0035) → igual, pagador el llamante; sin
     `userId`.
   - Service token → `service_token`; la relación `alia-kaana` en producción
     sigue siendo `internal_metered` con `capacity`, sin `reservation`.
     **Pro/Ultra de Alia**: `steps = [{level: pro|ultra, limits:{maxFundingTier:'credit',
     maxPricePerRequest: 1.00 USD}}, {level: 'xhigh', limits:{admittedModelLines: tuplas del piloto,
     maxPricePerRequest: 0.05 USD}}]`: Kaana intenta el primer escalón solo
     con claves de crédito y, si no hay ninguna admisible, el segundo; nunca
     Claude de pago. Es la misma semántica que `inferenceEdge.service.ts:1561-1574,2270-2279`.
   - Agente de producto en Alia → Alia no puede reenviar el token del
     producto porque su audiencia no es `kaana.ai` (decisión 6). Oxy añade un
     **intercambio de token** (`POST /auth/service-token/exchange`, RFC 8693)
     que solo el workload de Alia puede llamar y que emite un token
     `aud: kaana.ai` del **mismo** `applicationId` y `credentialId` del
     producto, con su `userId`. Pagador: el dueño de la app de producto, como
     hoy.
   - Sesión de usuario de una app Oxy (credencial pública nueva) →
     `product_session`: pagador el dueño de la app del `clientId` de la
     sesión, `userId` el de la sesión (verificado, no solo declarado),
     `commercial`. Es la semántica del carril de Inbox; ningún usuario paga
     de su saldo. Inbox migra reenviando el bearer de sesión del usuario: su
     `clientId` debe resolver a la app Inbox `6a37b3e61ddfd195b656819b` para
     que el pagador no cambie (se comprueba en la fase 2).
   - Permiso scoped → `reservation.fundingRestriction: 'promotional-only'` +
     `exactRoute`.
   - Jev y Private Auto → `internal_metered` cuando la relación lo dice, con
     `exactRoute` y el hash canónico de entrada; Kaana calcula ese hash con la
     función publicada en `@kaana.ai/contracts` y Oxy lo compara con la
     aprobación.
   - BYOK → `platform_fee_only` + `customerProviderCredential`.
   - **El precio al cliente no depende de lo que elija Kaana**: Oxy reserva
     con el máximo de cotización de `admittedModelLines` y liquida con el
     `priceVersionId` de la línea servida, que la concesión ya contiene; si
     Kaana sirve fuera de la concesión, Oxy liquida como hoy y registra
     `policy_violation`. El tier servido sigue siendo un dato de coste
     (`usage.fundingTier`) y alimenta solo `fundingCeilingBreach`.

5. **Kaana resuelve los niveles de potencia en este mismo corte** (Kaana ADR
   0001 decisión 4; Oxy ADR 0038 fase 2).
   - **Se mueve a Kaana:** la pertenencia a niveles (la tabla
     `inference_model_power_classes` pasa a `configs/power-classes.json`,
     revisada en git, «declared, never derived» como
     `configs/model-attribution.json`, con las mismas columnas de evidencia;
     su contenido se exporta de la base de datos de producción de Oxy en el
     corte, no de la semilla 0129); `POWER_LEVEL_CLASS`, `powerClassModelIds`,
     `powerLevelCandidates`, la deduplicación high/xhigh, la preferencia por
     no razonar en `instant`, `resolvePowerLevelEffort` y el mismo esfuerzo en
     failover; `auto` (`classifyAutoPowerLevel`, umbrales, escalera, techo
     `xhigh`); los scorecards de financiación, `fundingPriorityFor`,
     `kaanaReviewedProviderRoutes.ts` y la renovación de scores (que
     desaparecen: Kaana ordena por sus propias claves). El clasificador Jev
     sigue dormido; cuando se active corre dentro de Kaana y su coste se
     liquida dentro de la concesión del padre con su propio techo
     (USD 0,001, `O:services/inferenceAutoPowerLevel.service.ts:17-24`).
   - **Cómo se calcula en Kaana:** nivel → clase → líneas de modelo con
     revisión actual servible → ∩ `admittedModelLines` de la concesión → todas
     sus (proveedor, clave) ordenadas free → crédito → pago, más barato
     dentro del tier, luego salud (ADR 0001 decisión 3). Esto cambia un orden:
     en Oxy `discounted_payg` iba antes que `promotional_credit`; con este
     corte va después, como manda ADR 0001.
   - **Se queda en Oxy:** la política (`allowedRoutingProfileIds` con los ids
     estables `power-*`, `defaultTarget`, topes, fallback, BYOK), las listas
     de precios por línea de modelo, holds, recibos y la detección de
     violaciones, el carril de Oxy ADR 0037 como política, y las tablas
     `inference_routing_profiles`.
   - **Catálogo:** Kaana publica la pertenencia (`powerClass` por línea de
     modelo) en `GET /internal/v1/models` (que Oxy sincroniza para Console y
     para calcular `admittedModelLines`) y en `GET /v1/models` y
     `GET /v1/power-levels`.
   - **Sin modo sombra en producción** (sería un doble camino): antes del
     corte se reproduce fuera de línea la resolución de Kaana sobre los
     hechos sin contenido de la telemetría de Oxy y se compara con lo que
     Oxy eligió; las diferencias explicadas por el cambio de orden se
     aceptan, las demás bloquean.
   - **Tests.** Se portan a Go y se borran en Oxy:
     `O:packages/api/src/services/__tests__/{inferencePowerLevels,inferenceAutoPowerLevel,inferenceAutoClassifierChild}.test.ts`,
     `inferenceAutoAdmission.integration.test.ts`. Se reescriben para la
     concesión: `routes/__tests__/{inferenceEdgePowerLevels,inferenceEdgeInternalMetered}.test.ts`,
     `services/__tests__/{inferenceRoutingPolicyPowerLevels,inferenceInternalPilot,inboxInference.service}.test.ts`,
     los cuatro de catálogo (`inferenceCatalogue*.test.ts`) y
     `O:packages/contracts/src/__tests__/inference.{powerLevels,routingPolicy}.test.ts`
     (se mudan a `@kaana.ai/contracts`). Se borran con su código:
     `config/__tests__/inferenceRoutingScoreValidity.test.ts`,
     `db/schema/__tests__/routingScoreEconomicsCallsites.test.ts`. Se quedan:
     los de política (`inferenceRoutingPolicies.test.ts`,
     `inferenceRoutingPolicy.service.test.ts`, Console
     `routing-policy-form.test.tsx`, `routing-policy.test.ts`). En Kaana:
     quitar las exclusiones de `internal/contract/descriptor_test.go:357-358`;
     revisar `request_authorized_routes_test.go`, `fixtures_test.go`,
     `internal/kaana/{executor,failover}_test.go`, `internal/httpapi/server_test.go`
     y los de `private_auto`; nuevos: carga de `power-classes.json` (como
     `internal/publisher/checked_in_attribution_test.go`), selección por
     nivel, regla de esfuerzo, `auto`, y escalones con techo de financiación.

6. **Credenciales en `kaana.ai/v1`** (las resuelve Oxy en `authorizations`):
   `Authorization: Bearer` con una `oxy_sk_` de Console; un service token de
   Oxy con **audiencia dedicada `aud: kaana.ai`** (Oxy emite tokens para esa
   audiencia; Kaana rechaza `oxy-api`), con `X-Oxy-User-Id` opcional; o una
   sesión de usuario de una app Oxy con `inference:invoke` consentido. `alia_sk_*`
   sigue retirado. CORS abierto en `/v1/*` para el origen de Console (el
   playground) y para los orígenes registrados de cada app que use sesiones.

7. **Superficie pública `https://kaana.ai/v1`** (una sola versión, sin alias;
   el borde viejo se borra entero, así que todo lo que sirve hoy entra en el
   primer corte):

   | Método y ruta | Notas |
   |---|---|
   | `GET /v1/models`, `GET /v1/models/{publisher}/{model}` | catálogo con modalidades, clase de potencia, límites, parámetros, voces, idiomas, formatos, rango de `speed`, máximo de caracteres y **precios** (la lista de Oxy) |
   | `GET /v1/power-levels` | niveles visibles para la credencial y sus líneas de modelo; sustituye a `/v1/models/routing-profiles` |
   | `POST /v1/responses` | nativo; SSE si `stream: true` |
   | `POST /v1/chat/completions` | compatible con el SDK de OpenAI (`baseURL: 'https://kaana.ai/v1'`, `apiKey: 'oxy_sk_…'`) |
   | `POST /v1/embeddings` | ya en el contrato (`internal/contract/embeddings.go`) |
   | `POST /v1/audio/speech` | `stream: false` → archivo; `stream: true` → audio en streaming (decisión 8) |
   | `POST /v1/audio/transcriptions` | multipart, ≤ 20 MiB, síncrono |
   | `POST /v1/images/generations` | lo que hoy sirve Oxy |
   | `POST /v1/decisions` | familia dormida; conserva sus revisiones pendientes (`docs/decisions.md:3-10,122-128`) |
   | `GET /v1/generations/{id}`, `GET /v1/generations?idempotency_key=` | recibo de uso y coste; Kaana lo sirve con el importe que Oxy devuelve en una autorización `receipt.read` (`inference:usage:read`) |
   | `GET /v1/realtime?model=` | WebSocket; primer frame `session.open`/`session.resume`; una concesión por sesión, liquidación al cerrar |
   | `GET /livez` | sin cambios |

   Se conservan las reglas de hoy: el estado HTTP solo decide antes del
   primer byte; después, todo fallo es el evento terminal `error`, tipado,
   nunca un relleno con `finish_reason: "stop"` (`server.go:15-27`). Ids
   `publisher/model[@revision]` sin sustitución; niveles sin `/`. **Nunca se
   exponen proveedor, operador, clave ni `deploymentId`** en respuestas,
   errores, cabeceras ni recibos públicos. Cabeceras `Kaana-Request-Id` y
   `Kaana-Contract-Version`.

8. **`speechStream` entra en el contrato nuevo.** Se apoya en el trabajo de
   `feat/speech-stream` (los adaptadores ya entregan el audio según llega):
   este ADR solo le da forma pública. `POST /v1/audio/speech` con
   `stream: true` y `stream_format`:
   - `"audio"` (compatible con OpenAI): `200`, `Content-Type: audio/mpeg` o
     `audio/pcm;rate=24000`, `Transfer-Encoding: chunked`, bytes crudos. Un
     fallo antes del primer byte es un estado HTTP con cuerpo de error; uno
     posterior corta la conexión y el recibo guarda el código.
   - `"sse"` (el del SDK): `speech.audio.delta` (base64), `speech.audio.done`
     (con `usage`) y `error`, para que un fallo a mitad llegue tipado.
   El contrato interno pasa a admitir `audio_speech` con `stream: true`
   (hoy prohibido, `request.go:438-439`) en el mismo PR de contrato de la
   fase 1. El primer byte baja de verdad en cada proveedor cuando su
   adaptador use su endpoint de streaming.

9. **Límites de voz publicados y validados antes de reservar.** Por cada
   modelo de voz, el publisher publica en `inventory.observed`:
   `voices[] {id, name, languages[], gender?}`, `languages[]` (BCP 47),
   `audioFormats {input[], output[]}`, `speed {min, max}` (ausente si el
   adaptador no admite `speed`) y `maxInputCharacters` (UTF-16, como hoy
   cuentan los adaptadores). `inventory.Parse` los valida y `GET /v1/models`
   los sirve. Si una línea de modelo la sirven varios proveedores, el valor
   publicado es la **intersección** (lo que todos aceptan). El handler de
   `/v1/audio/speech` los comprueba **antes** de pedir la concesión: un
   `speed` 1,5 a un modelo ElevenLabs (0,7–1,2) es `invalid_request` con
   `param: "speed"` sin reserva. Alia deja de fijar 0,7–1,5 en
   `routes/v1/audio.ts:45` y valida contra el catálogo.

10. **El contrato se muda a Kaana como `@kaana.ai/contracts`.**
    - Fuente: zod 4 en `packages/contracts/src/` de este repo. Shapes
      públicas (peticiones y respuestas de `/v1/*`, eventos, errores,
      catálogo, realtime, speech, transcripción, decisions, recibos, niveles)
      y, en `@kaana.ai/contracts/internal`, las de Oxy↔Kaana (concesión,
      liquidación, bandeja de salida, plano de control, hash canónico de
      entrada). Las peticiones públicas de
      `O:packages/api/src/schemas/inferenceEdge.schemas.ts` se mudan aquí.
    - Se quedan en `@oxy.so/contracts`: facturación, ledger, precios,
      política de enrutado como recurso de Console, entitlements, `inbox.ts`
      (producto de Oxy, `/email/ai/*`) y `aliaModelRelease`.
      `O:packages/contracts/src/inference/` desaparece.
    - `tools/contract/generate.mjs` deriva `descriptor.json` del paquete del
      **workspace** (fuente = commit), no de npm; los tests Go siguen
      comparando campo a campo. Dirección: **zod → descriptor → Go**. Los
      tipos TypeScript son `z.infer` de la fuente; no se generan desde el
      descriptor, que no expresa refinamientos ni reglas cruzadas.
    - Se genera `openapi.json` (OpenAPI 3.1, `z.toJSONSchema`) para clientes
      de otros lenguajes; CI regenera y falla ante cualquier diff.
    - Contract set **4.0.0**.
    - **Vocabulario público de errores:** `provider_credential_invalid`,
      `provider_billing_refused` y `deployment_unavailable` existen solo en
      `@kaana.ai/contracts/internal`. En `/v1` Kaana retira la clave y sigue;
      si no queda ruta, el cliente recibe `service_unavailable` (reintentable)
      o `no_route_available`. `byok_credential_invalid` sí es público: es la
      clave del propio cliente. Ningún mensaje nombra proveedor ni operador.

11. **SDK `@kaana.ai/sdk` y contratos, en este repo** (`packages/sdk`,
    `packages/contracts`, bun workspaces con `tools/contract`). Biome pasa a
    cubrir `packages/**` y `tools/**` (cambio explícito de la regla de
    `AGENTS.md`). Sin dependencias de Oxy: la credencial es una función.
    **Publicación:** `OxyHQ/Kaana` es público, así que el límite de secretos
    de organización de GitHub Free no aplica y npm admite **trusted
    publishing (OIDC)** con procedencia. Workflow `release-packages.yml`
    (`workflow_dispatch` con commit exacto de `main`), mismo patrón de dos
    trabajos que Oxy: `build` produce los tarballs con `bun pm pack` y los
    sube como artefacto; `publish` (`permissions: id-token: write`, entorno
    protegido `npm`) reconstruye, compara bytes y ejecuta
    `npm publish <tgz> --provenance --access public`. Sin `NPM_TOKEN`
    permanente. Si npm exige que el paquete exista antes de configurar el
    publicador de confianza, la **primera** versión de cada paquete se
    publica una vez con un token granular limitado al scope `@kaana.ai` y de
    caducidad corta, como secreto del repositorio, que se borra en cuanto
    OIDC queda configurado. *(2026-10-16: los dos paquetes ya tienen su
    publicador de confianza; `release-packages.yml` llama al `npm-release.yml`
    compartido de `OxyHQ/oxy`, el mismo para todos los repos de OxyHQ, y las
    puertas de cada paquete son su `prepublishOnly` —
    `O:docs/engineering/package-rules.md#publishing`.)*

12. **Documentación para developers** vive en Oxy: Console
    (`routes/_layout/documentation/*`) y `O:docs/inference/*`, apuntando a
    `https://kaana.ai/v1` y `@kaana.ai/sdk`. Kaana publica la referencia
    generada (`openapi.json`, README del SDK) y enlaza a Console para cuentas,
    claves, límites y facturación.

13. **Corte limpio, sin aviso a terceros.** Ningún consumidor lleva flag,
    fallback ni doble cliente. `api.oxy.so/v1/{responses,chat/completions,
    audio/speech,images/generations,decisions,generations,models,realtime}` y
    `/models` se borran, sin redirección ni periodo de gracia. El alias
    `api.kaana.ai` se retira en el mismo corte.

## Arquitectura de la petición

```text
cliente (@kaana.ai/sdk, SDK de OpenAI, curl, playground de Console)
  │  HTTPS / WSS  Authorization: Bearer <oxy_sk_… | service token aud=kaana.ai | sesión>
  ▼
Cloudflare (proxy de kaana.ai; rate limit por IP en /v1/* y en /internal/*)
  ▼
ALB dedicado de Kaana → servicio ECS `kaana` (≥ 2 tareas)
  │ 1. límite de cuerpo; parseo con @kaana.ai/contracts; validación contra el
  │    catálogo (parámetros, voz, idioma, formato, speed, caracteres)
  │ 2. hechos sin contenido + sha256 del cuerpo
  │──── POST api.oxy.so/internal/kaana/v1/authorizations ─────────▶ Oxy
  │        (Kaana como workload, ADR 0026)                        │ credencial → principal y carril
  │                                                               │ política de Console, rate limits, capacidad
  │                                                               │ cotiza y reserve() (límites de gasto)
  │◀──────── concesión firmada Ed25519 (oxy-kaana-grant:v1) ───────┘
  │ 3. verifica concesión y sha256
  │ 4. nivel → líneas de modelo (∩ admittedModelLines) → deployments →
  │    pool de claves free → crédito → pago, dentro de maxFundingTier;
  │    reintento y failover antes de la salida; escalón siguiente si no hay ruta
  ▼
proveedor
  │ stream → Kaana normaliza → SSE / audio chunked / WebSocket al cliente
  ▼
Kaana: usage report + provider_cost_events + fila de bandeja (una transacción)
  │──── POST api.oxy.so/internal/kaana/v1/settlements (firma Kaana) ──▶ Oxy settle()
  └──── GET kaana.ai/internal/v1/settlements?cursor= ◀── reconciliación de Oxy
```

| Paso | Dónde | Notas |
|---|---|---|
| TLS, rate limit por IP | Cloudflare | reglas nuevas para `/v1/*` |
| Límite de cuerpo y validación contra catálogo | Kaana | antes de reservar; hoy Kaana lee hasta 16 MB antes de verificar (`I:app-kaana.tf:483-489`) |
| Autenticación de la credencial | Oxy | Kaana nunca guarda ni interpreta `oxy_sk_` |
| Política de Console, rate limits, capacidad | Oxy | hoy `admitRequest`, `machineCredentialLimiter`, `claimMeteredAdmission` |
| Reserva y límites de gasto | Oxy (`reserve`) | dimensionado con los hechos de Kaana |
| Nivel, modelo, deployment y clave | Kaana | ADR 0001; `auto` por contenido solo puede vivir aquí |
| Ejecución, streaming, cancelación | Kaana | |
| Medición | Kaana | unidades, nunca importes |
| Liquidación y factura | Oxy (`settle`) | desde el recibo de Kaana |
| Recibo al cliente | Kaana sirve, Oxy pone el importe | |

### De dónde lee Oxy hoy la política de Console y cómo llega a Kaana

| Política (se configura en Console) | Dónde la lee Oxy hoy | Cómo llega a Kaana |
|---|---|---|
| Objetivo por defecto, niveles y modelos permitidos, topes de precio, proveedores, regiones, ZDR, licencias, fallback, BYOK | `resolveEffectiveRoutingPolicy` (`O:services/inferenceRoutingPolicy.service.ts:965`), llamada en `inferenceEdge.service.ts:1063-1067`; niveles permitidos en `:1505-1531`; campos en `O:docs/inference/routing.md:93-108` | `steps[].target` y `steps[].limits` de la concesión; `routingPolicyVersionId` como procedencia |
| Límites de gasto y presupuestos (cuenta-subárbol, app, credencial) | `evaluateSpendingLimits` (`O:services/spendingLimit.service.ts:141`) dentro de `reserve` (`inferenceLedger.service.ts:671-683`) | no viajan: si no caben, no hay concesión (`spending_limit_exceeded`) |
| Saldo y modo de facturación | `resolveBillingAccount`, `computeDraw` (`inferenceLedger.service.ts:173-222,1672-1711`) | no viajan; `reservation.ceiling` |
| Rate limit por credencial y por app | `O:middleware/machineCredential.ts:347-361` | lo aplica Oxy en `authorizations` (`rate_limited` con `retryAfterMs`) |
| Capacidad de relaciones internas | `O:config/inferenceEconomicPolicy.ts:138-197`; `claimMeteredAdmission` (`inferenceEdge.service.ts:2132-2170`) | `capacity.meteredUsageId`; se libera al liquidar |
| Audiencia del catálogo (`public` o `platform_internal`) | `applications.type` e `isInternal` (`inferenceEdge.service.ts:533-552`; `O:services/inferenceCatalogue.service.ts:204-214`) | autorización sin reserva `catalogue.read`, cacheada 60 s por credencial; Kaana filtra `GET /v1/models` y `/v1/power-levels` con la respuesta |

Latencia: un salto Kaana→Oxy antes del primer token, en lugar del salto
cliente→Oxy→Kaana de hoy; ambos en `us-west-2`.

## Superficie del SDK (`@kaana.ai/sdk` 1.0.0)

```ts
import type * as C from '@kaana.ai/contracts';

/** Valor de `Authorization: Bearer`. Se llama en cada petición; el SDK no
 *  cachea: quien da la función (OxyServices, OxyServer) ya cachea y renueva. */
export type KaanaCredential = string | (() => string | Promise<string>);

export interface KaanaOptions {
  credential: KaanaCredential;
  /** Solo `https://kaana.ai` o loopback en desarrollo. */
  baseURL?: string;
  fetch?: typeof globalThis.fetch;
  /** Para realtime fuera de navegador/Bun (p. ej. `ws` en Node). */
  WebSocket?: typeof globalThis.WebSocket;
  timeoutMs?: number;   // 120_000
  maxRetries?: number;  // 2; solo antes del primer byte y con códigos reintentables
}

export interface RequestOptions {
  signal?: AbortSignal;
  idempotencyKey?: string;      // Idempotency-Key
  /** Atribución delegada (X-Oxy-User-Id); solo con un service token. */
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
    list(options?: RequestOptions): Promise<C.PowerLevelEntry[]>;
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

/** Eventos validados; `final` resuelve con la respuesta plegada o rechaza con
 *  el KaanaError del evento terminal `error`. */
export interface KaanaStream<E, F> extends AsyncIterable<E> {
  readonly requestId: Promise<string>;
  readonly final: Promise<F>;
  abort(reason?: unknown): void;
}

export interface SpeechResult {
  audio: Uint8Array;
  mediaType: C.AudioMediaType; // 'audio/mpeg' | 'audio/wav' | 'audio/ogg' | 'audio/aac' | 'audio/flac' | 'audio/pcm'
  model: C.ModelReference;     // lo que realmente sirvió
  requestId: string;
  usage: C.Usage;
}

/** Audio en orden, en cuanto el proveedor lo produce (`stream_format: 'sse'`,
 *  para que un fallo a mitad llegue tipado). */
export interface SpeechStream extends AsyncIterable<Uint8Array> {
  readonly mediaType: Promise<C.AudioMediaType>;
  readonly requestId: Promise<string>;
  toReadableStream(): ReadableStream<Uint8Array>;
  /** Modelo, uso y requestId al terminar; rechaza con KaanaError. */
  readonly done: Promise<Omit<SpeechResult, 'audio'>>;
  abort(reason?: unknown): void;
}

export interface TranscriptionRequest {
  model: C.ModelReference;
  file: Blob | Uint8Array;
  mediaType: C.AudioInputMediaType;
  language?: string;            // BCP 47, validado contra el catálogo
  prompt?: string;
  timestamps?: 'none' | 'segment' | 'word';
}

export interface RealtimeSession extends AsyncIterable<C.RealtimeServerEvent> {
  readonly sessionId: string;
  send(event: C.RealtimeClientEvent): void;
  appendAudio(pcm16: Uint8Array): void;
  resume(): Promise<void>;      // ventana de 30 s (docs/realtime.md)
  close(): Promise<C.Usage>;    // liquida exactamente una vez
}

/* Errores tipados; `code` es el vocabulario PÚBLICO de @kaana.ai/contracts
 * (INFERENCE_ERROR_CODES de hoy sin los códigos de proveedor, decisión 10). */
export class KaanaError extends Error {
  readonly code: C.PublicErrorCode;
  readonly status: number | null;   // null si llegó como evento terminal
  readonly requestId: string | null;
  readonly retryable: boolean;
  readonly retryAfterMs?: number;
  readonly param?: string;
  readonly afterOutput: boolean;    // hubo salida antes del fallo
}
export class KaanaAuthenticationError extends KaanaError {}  // authentication_failed (enlaza a Console)
export class KaanaPermissionError extends KaanaError {}      // permission_denied, insufficient_scope, commercial_permission_denied, policy_violation
export class KaanaInvalidRequestError extends KaanaError {}  // invalid_request, model_not_found, unsupported_modality, context_length_exceeded, request_too_large, output_limit_exceeded, idempotency_conflict, byok_credential_invalid
export class KaanaBillingError extends KaanaError {}         // insufficient_balance, spending_limit_exceeded, quota_exceeded
export class KaanaRateLimitError extends KaanaError {}       // rate_limited
export class KaanaUnavailableError extends KaanaError {}     // no_route_available, service_unavailable, provider_error, provider_timeout, provider_overloaded
export class KaanaContentFilteredError extends KaanaError {} // upstream_content_filtered
export class KaanaCancelledError extends KaanaError {}       // cancelled
export class KaanaInternalError extends KaanaError {}        // internal_error
/** Respuesta que no cumple @kaana.ai/contracts; nunca reintentable. */
export class KaanaProtocolError extends Error { readonly requestId: string | null; }
/** Red o timeout antes de cualquier respuesta. */
export class KaanaConnectionError extends Error { readonly cause: unknown; readonly timedOut: boolean; }
```

Credenciales (sin helpers en el SDK; quien tiene la identidad da la función):

```ts
new Kaana({ credential: process.env.KAANA_API_KEY! });                 // oxy_sk_ de Console
new Kaana({ credential: () => oxyServer.serviceToken({ audience: 'kaana.ai' }) }); // servicio Oxy; la opción `audience` es nueva (fase 2)
new Kaana({ credential: async () => (await oxy.getAccessToken())! });  // sesión de app Oxy
```

## Plan de corte por fases

«Desplegado» significa leído de vuelta en producción, no mergeado
(`~/Oxy/docs/kaana-inference.md`, «Source state is not production state»).

**Fase 0 — Decisión y documentación.** Este ADR aceptado; Oxy ADR 0039 y
Alia ADR 0016; notas de «superseded» al principio de los ADR sustituidos.

**Fase 1 — Contrato y niveles en Kaana** (repo Kaana).
1. `packages/contracts` (`@kaana.ai/contracts` 4.0.0): shapes públicas e
   internas, concesión con escalones, liquidación, `audio_speech` con
   `stream: true`, transcripción, voces/idiomas/formatos/`speed`/caracteres,
   clase de potencia, vocabulario público de errores, hash canónico de
   entrada. Descriptor desde el workspace; Go `internal/contract`;
   `openapi.json`.
2. `configs/power-classes.json` exportado de producción de Oxy; resolución
   de niveles, esfuerzo y `auto` en Go con sus tests; publisher que publica
   clase de potencia y límites de voz. Merge de `feat/speech-stream` antes o
   junto a este paso.
3. Biome a `packages/**` y `tools/**`; workflow `release-packages.yml`
   (OIDC). Publicar `@kaana.ai/contracts@4.0.0`.
4. Repetición fuera de línea de la resolución de niveles contra la
   telemetría de Oxy (decisión 5).

**Fase 2 — API interna en Oxy** (repo OxyHQServices). Depende de 1.3.
1. `/internal/kaana/v1/authorizations` y `/settlements`, reutilizando
   `authenticateEdgeCaller`, `admitRequest`, la política, la capacidad, los
   rate limits, el dimensionado, `reserve` y `settle`; concesión con la
   matriz de la decisión 4; cálculo de `admittedModelLines` desde la
   pertenencia publicada por Kaana.
2. Service tokens con `aud: kaana.ai`; intercambio de token para el carril de
   agentes de producto de Alia; sesiones de usuario como `product_session`
   (comprobar que la sesión de Inbox resuelve a la app Inbox).
3. Lista de precios en el catálogo; verificación de la firma de
   liquidación; reconciliación por cursor.
4. Desplegado sin llamador. El borde viejo sigue sirviendo hasta la fase 5.

**Fase 3 — Servidor público y SDK** (repos Kaana y oxy-infra). Depende de 2.4.
1. `internal/publicapi`: `/v1/*`, validación contra catálogo, cliente de
   autorización, concesión, escalones, bandeja de liquidación (migración
   nueva en `internal/credentialstore/migrations`), CORS, límite de cuerpo,
   enlaces a Console.
2. `@kaana.ai/sdk@1.0.0` con tests contra un servidor de prueba del mismo
   contrato; publicar.
3. oxy-infra: `desired_count ≥ 2` y autoescalado; reglas Cloudflare de rate
   limit para `/v1/*`; revisar que TTFB 125 s y `idle_timeout` 900 s cubren
   speech streaming y realtime.
4. Canario de extremo a extremo con una clave de Console de prueba, leído de
   vuelta: concesión → ejecución → liquidación → recibo, para cada carril de
   la matriz (al menos `oxy_sk_`, `alia-kaana` interno con Pro bajo crédito,
   agente de producto, sesión, BYOK si hay alguno activo).

**Fase 4 — Consumidores** (en paralelo tras 3.4; cada uno quita
`@oxy.so/core/inference` y añade `@kaana.ai/sdk` en el mismo commit, con
`bun.lock`). Orden de merge:
1. **Homiio** (un `respond`).
2. **OxyHQServices**: playground de Console (`use-playground.ts` →
   `@kaana.ai/sdk` desde el navegador con la `oxy_sk_` en memoria, más
   `playground-receipt.tsx`), docs de Console
   (`routes/_layout/documentation/*`), `inboxInference.service.ts` (deja de
   llamar en proceso a `executeInferenceRequest`; usa el SDK con la sesión
   del usuario), smoke `.github/scripts/decisions-release-smoke`, canario
   `packages/api/scripts/run-kaana-signed-canary.mjs`.
3. **Mention** (`respond`, `decide`, `getGenerationRecordByIdempotencyKey`;
   aprobaciones Jev reemitidas con el hash canónico) y su gate.
4. **Alia** (`respond`, `stream`, `speech`, `listModels` → `models.list` y
   `powerLevels.list`, errores tipados; `speed` validado contra catálogo;
   intercambio de token en el carril de agentes de producto). Un push a
   `main` de Alia despliega con migraciones: `test:pg` en local antes.
   Los gates de frontera (Alia `oxy-inference.ts:20`, Mention
   `scripts/validate-inference-boundary.mjs:151`, Syra
   `scripts/validate-ai-architecture.mjs:67`, Nilo
   `apps/api/src/db/__tests__/inferenceBoundary.test.ts:404-406`) se
   invierten en el mismo cambio: `https://kaana.ai` está permitido **solo**
   a través de `@kaana.ai/sdk`; `/internal/` y los SDK de proveedores siguen
   prohibidos.

**Fase 5 — Retirada** (el mismo día que el último despliegue de la fase 4).
1. OxyHQServices: borrar `packages/core/src/inference` y el subpath
   `./inference` → **`@oxy.so/core@5.0.0`**; borrar
   `packages/contracts/src/inference/` (mudando `inbox.ts` y facturación) →
   **`@oxy.so/contracts@5.0.0`**; borrar `routes/inferenceEdge.ts`,
   `inferenceRealtime.ts`, `/v1/models` y `/models` públicos,
   `schemas/inferenceEdge.schemas.ts`, la parte de ejecución de
   `httpKaanaClient.ts`, `kaanaRealtimeClient.ts`, el resolvedor de niveles y
   scorecards (decisión 5), `INFERENCE_MACHINE_CREDENTIAL_AUTH`,
   `INFERENCE_EDGE_AUDIENCE`; `packages/api/openapi.json` sin inferencia.
2. Kaana: borrar `POST /internal/v1/{inference,decisions}`,
   `GET /internal/v1/realtime`, los sobres v1–v4, el dominio
   `oxy-kaana-envelope:v1` y la regla Cloudflare de `/internal/v1/inference`.
   Quedan las rutas de plano de control.
3. oxy-infra: retirar `api.kaana.ai` (`I:app-services.tf:16-21,763-797`) y
   su DNS.
4. Docs del ecosistema (lista abajo); website: regenerar
   `src/content/_synced/index.json`.

Publicación: `@kaana.ai/contracts@4.0.0` (1.3) → `@kaana.ai/sdk@1.0.0` (3.2)
→ consumidores (fase 4) → `@oxy.so/core@5.0.0` y `@oxy.so/contracts@5.0.0`
(5.1).

## Enmienda 2026-10-15: el catálogo es de Kaana

Decisión del dueño, vinculante: «corte limpio, nada de tricky things». El
catálogo de modelos es de Kaana, y de nadie más. Esta enmienda corrige las
decisiones de arriba que daban a Oxy la identidad de catálogo y los precios
(decisión 7, «catálogo con … **precios** (la lista de Oxy)», y la fila «Se
queda en Oxy: … las listas de precios por línea de modelo»).

### Qué cambia

- **Fuente única.** Identidad (`modelId`, publisher, nombre), licencia,
  procedencia, política de datos, capacidades, voces, deprecación y **precio de
  lista por unidad** de cada modelo viven en un fichero revisado del repo de
  Kaana, `configs/public-catalogue.json`, igual que ya lo es
  `configs/model-attribution.json`. Un cambio es un commit revisado. Nada lo
  deriva y no existe una segunda copia.
- **Forma.** `declaredCatalogueSchema` / `declaredModelSchema` en
  `@kaana.ai/contracts/internal` (conjunto 4.3.0, aditivo): `modelSchema`
  menos lo que Kaana rellena al servir (`schemaVersion`, `currentRevision`,
  `availableRevisions`), más `audience`, y **estricto**. Un modelo declarado no
  lleva `servingProviders`, `availabilityScope` ni `commercialPermission`: son
  propiedades de una ruta, no del modelo, y las decide el inventario firmado y
  la concesión de Oxy. `ParsePublicCatalogue` los rechaza y
  `tools/contract/validate.mjs` parsea el fichero entero con el esquema Zod.
- **Dónde se sirve.** `GET /v1/models` y `/v1/power-levels` (como antes, el
  fichero ∩ lo que sirve el inventario) y, **entero y con precios**, como
  miembro `declaredCatalogue` de la firmada `GET /internal/v1/models`. Es
  todo lo declarado, esté servido ahora o no: el inventario dice qué se sirve y
  este miembro dice qué es y cuánto cuesta.
- **Imagen.** El Dockerfile hornea el fichero en `/etc/kaana-catalogue/` y fija
  `KAANA_PUBLIC_CATALOGUE_PATH` por defecto; la tarea de oxy-infra lo repite.
  Kaana lo carga siempre, esté o no habilitada la API pública, porque Oxy lo
  lee de la superficie interna.
- **Los precios son de lista.** Son el precio por unidad al que Oxy tasa una
  concesión, no el coste de Kaana (eso sigue siendo `providercost` y las
  tarifas de `provider-rates.json`) ni un importe de cliente. Ninguna respuesta
  de inferencia lleva un importe.

### Qué deja de hacer Oxy

- Deja de ser autoridad del catálogo: no tiene tabla de modelos, publishers,
  licencias, políticas de datos, capacidades, voces ni precios de lista, ni
  publica `GET /v1/models`.
- **Pone precio a las concesiones LEYENDO el catálogo de Kaana**
  (`declaredCatalogue` en `GET /internal/v1/models`, firmada, que ya consume
  para `admittedModelLines`). El `priceVersionId` de cada modelo declarado es
  el identificador de esa lista de precios: Oxy lo copia a la concesión. Un precio nuevo es un
  `priceVersionId` nuevo; uno publicado nunca cambia.
- Conserva lo económico y la gestión: holds, recibos, política y topes de
  Console, BYOK, descuentos y márgenes sobre el precio de lista. Una concesión
  para un modelo sin `pricing` declarado no se puede tasar por unidad: Oxy la
  rechaza (es el caso BYOK-only), nunca la tasa a cero.

### Cómo se migran los datos

El volcado del catálogo de Oxy (252 modelos, tomado el 2026-10-15) es una
**migración de una sola vez**, no una segunda fuente: se hizo una exportación,
se corrigió para ajustarse al contrato y se revisó como un commit. Desde ese
commit el fichero de Kaana es el único que se edita. Correcciones hechas en la
migración (están en el `$comment` del fichero):

- se quitan `servingProviders`, `availabilityScope` y `commercialPermission`
  (describen la ruta; `servingProviders` nombraría al operador en la superficie
  pública);
- el id de licencia `LicenseRef-Oxy-Serving-Provider-Terms` pasa a
  `LicenseRef-Kaana-Serving-Provider-Terms`;
- todas las entradas exportadas conservan `audience: platform_internal` hasta
  una revisión comercial; `public` es una decisión del dueño por modelo.

Después de la exportación se declaran, con fuente primaria citada, los modelos
de transcripción que Kaana ya sirve: `openai/gpt-4o-transcribe`,
`openai/gpt-4o-mini-transcribe-2025-03-20` y `…-2025-12-15` (precio por
tokens, política de datos de OpenAI, deprecaciones publicadas). Quedan
**fuera por falta de evidencia de un campo obligatorio**: `openai/gpt-transcribe`
y `openai/whisper-1` (el precio por minuto está publicado, pero el contrato
exige `maxContextTokens`/`maxOutputTokens` y OpenAI no los declara) y todo
Deepgram (el adaptador no publica inventario y `model-attribution.json` no lo
atribuye). `x-ai/grok-voice-think-fast-2.0` lleva precio por las cuatro
unidades que mide su adaptador realtime (`audio_input_milliseconds`,
`audio_output_milliseconds`, `session_milliseconds`, `requests`).

### Qué se retira de Oxy en la fase 5

Se añade a la lista de la fase 5, punto 1 (OxyHQServices):

- el servicio de catálogo (`services/inferenceCatalogue.service.ts`) y sus
  tablas de modelos, publishers, revisiones, despliegues comerciales y
  **versiones de precio por línea de modelo**, tras importar los
  `priceVersionId` ya usados por recibos vigentes (la historia de facturación
  conserva su copia; el catálogo vivo no);
- la exportación del volcado y cualquier tarea de sincronización que copie
  catálogo desde Kaana a tablas propias de modelos: Oxy lee `declaredCatalogue`
  al tasar y lo cachea en memoria con la versión de contrato, no lo persiste;
- los esquemas de catálogo de `@oxy.so/contracts` que `@kaana.ai/contracts`
  ya posee (`modelCatalogueEntrySchema` y vecinos);
- las pantallas de Console que editan identidad, licencia o precio de lista
  de un modelo: Console muestra el catálogo de Kaana y gestiona clientes,
  facturación y límites.

Lo que Oxy necesita para leer los precios está en `docs/operating.md`, «El
catálogo declarado».

## Enmienda 2026-10-18: decisions es una concesión ordinaria

Decisión del dueño: una aprobación estable en lugar de permisos de un solo
post. Contract set 7.0.0 retira de la decisión 4 el `exactRoute` de la
concesión y el `canonicalInputSha256` de la autorización (con `canonicalJson`),
y con ellos los sobres v3 (scoped) y v4 (Private Auto), la ruta firmada
`POST /internal/v1/decisions` y toda aprobación compilada en el código. Una
petición de decisions es una concesión de pasos ordenados como cualquier otra:
`model` es opcional (fijado si está; si falta, Oxy concede el perfil de
decisions de la aplicación, línea a línea), y cada ruta de decisions necesita su
tarifa revisada en `configs/provider-rates.json`, que es su techo de precio; sin
tarifa no se sirve. Jev se publica como un deployment ordinario de OpenRouter con
su `canonical_slug` fechado; el adaptador de la Decisions API de OpenAI
(`openai-decisions`) queda construido sin clave ni deployment. Detalle:
`docs/decisions.md`.

## Consumidores del corte en `origin/main` (2026-10-15)

`git grep` de `OxyInferenceClient|createInferenceClient|@oxy.so/core/inference`
y de llamadas al borde sobre la rama por defecto de los 35 repos de `~/Oxy`
(sin `docs/audits/**`, que solo contiene hashes del paquete).

| Repo | Ficheros (no test) | Usa |
|---|---|---|
| Alia | `packages/api/src/lib/inference/oxy-inference.ts:10,20,93-138`; `oxy-inference-credential.ts:45`; `kaana-language-model.ts:43-44,610,651`; `kaana-text.ts:86,100`; `lib/models/catalogue.ts:277-280`; `lib/synthesize-speech.ts:28-34`; `lib/errors/failover-error.ts:18`, `lib/inference/request-budget.ts:72`, `tool-limit.ts:35`, `routes/suggestions.ts:3`, `routes/v1/audio.ts:2,45` | `respond`, `stream`, `speech` (`delegatedUserId`), `listModels`, errores; reenvío de service token de producto |
| Mention | `packages/backend/src/utils/oxyInference.ts:2-5,27-30,86`; `services/contentClassification/jevProduction.ts:4,57,111`; `jevSdk.ts:4,27-28,51,68`; `jevReceipt.ts:16,130,140`; `controllers/posts/translation.ts:9`; `scripts/validate-inference-boundary.mjs:151` | `respond`, `decide`, `getGenerationRecordByIdempotencyKey`, errores |
| Homiio | `packages/backend/services/oxyInferenceService.ts:2-6,72,109-113`; `routes/ai.ts:26` | `respond`, errores |
| OxyHQServices | `packages/core/src/inference/*`; **Console playground** `packages/console/src/hooks/use-playground.ts:150-156,246-253` y `components/playground/playground-receipt.tsx` (`fetch` a `api.oxy.so/v1`); docs de Console `routes/_layout/documentation/*`; `services/inboxInference.service.ts:38,242,250` (en proceso); fixture `routes/__fixtures__/mentionWire.fixture.ts:287`; smoke `.github/scripts/decisions-release-smoke/smoke.{cjs,mjs}` | todo |
| website | `src/content/_synced/index.json:30532-30534,33038-33040` (docs sincronizados) | — |

Sin uso del cliente: Allo, Atlas, Clarity (vía Alia), Nilo/CRM (mismo repo;
solo gates y docs), CrowdSource, GoWay, Inbox (frontend → `api.oxy.so/email/ai/*`),
Mercaria (vía agente de Alia), Moovo, Move, Noted, Peable, Schedio, Syra
(gate y docs), tnp, Willo y el resto. Tipos que pasan a `@kaana.ai/contracts`:
Alia (`InferenceMessage`, `InferenceStreamEvent`, `InferenceErrorCode`,
`InferenceContentPart`), Mention (`DecisionRequest`, `DecisionSuccess`,
`InferenceMessage`, `InferenceRequestOutcome`, `inferenceEnvironmentSchema`,
`UsageSource`, `UsageQuantity`), Homiio (`InferenceMessage`,
`InferenceContentPart`), Console (`InferenceError`, `InferenceMessage`,
`use-playground.ts:2`). Inbox importa `inboxInference*`, que se queda en Oxy.

## Qué queda sustituido

- **Oxy ADR 0006** (`docs/adr/0006-oxy-kaana-boundary.md:60,123-130`): el
  borde público pasa a Kaana; la gestión y la experiencia de developer
  siguen en Console; Kaana sigue sin autorizar clientes porque decide Oxy.
- **Oxy ADR 0005** se mantiene: Oxy es el único plano de control.
- **Oxy ADR 0015**: el mecanismo de firma se conserva; lo firmado es la
  concesión.
- **Oxy ADR 0038 fase 2** y **Kaana ADR 0001 decisión 4 / fase 2**: se
  ejecutan en este corte sin modo sombra en producción y con concesión en
  lugar del sobre v5.
- **Alia ADR 0001**, **ADR 0010** (frase «generic inference is Kaana through
  Oxy»), ADR 0012 y 0014 (citan `OxyInferenceClient.listModels()`).
- `~/Oxy/AGENTS.md:19-21` (añadir «API pública `kaana.ai/v1`, gestión en Oxy
  Console») y `~/Oxy/docs/kaana-inference.md` (diagrama
  `app -> Oxy inference edge -> Kaana`; «Oxy … signs an ordered list of opaque
  deploymentIds»; «Oxy owns economic route priority»).
- `~/Oxy/docs/project-map.md:9-12`, `~/Oxy/docs/alia-agency-capability-architecture.md`.
- Alia `AGENTS.md:36-38`, `README.md:90`, `docs/chat-runtime.mdx:35`,
  `docs/model-abstraction.mdx:90,198,231`, `docs/deployment.md:170`,
  `docs/api-reference.md:300`, `docs/onboarding.md:133`,
  `docs/runbooks/kaana-cutover.md`, `docs/runbooks/rollback.md:5`,
  `packages/api/.env.example:78`.
- OxyHQServices `AGENTS.md:3-5,70`, `docs/inference/{README,sdk,request-routing,realtime,streaming,alia,migration,power-levels,routing}.md`,
  `docs/architecture/inference-responsibility-matrix.md`, docs de Console.
- Kaana `AGENTS.md:4-6`, `README.md:20,138,187`, `docs/architecture.md:7-17`,
  `docs/identity-and-routing.md:33-40`, `docs/contract.md:3-13`,
  `docs/routing.md:55-62`, `docs/decisions.md:3-10`, `docs/realtime.md`,
  `docs/operating.md:907-916` (ya desfasado: cita una zona privada de Route
  53 que oxy-infra eliminó).
- Clarity `AGENTS.md:6,16`; Nilo `AGENTS.md:22-25`, README, CONTRIBUTING;
  Noted `AGENTS.md:58-63`, README, `docs/index.mdx:16-17`; Syra
  `docs/ai-routing.md:3-16`, `.github/workflows/quality.yml:143-145`; Mention
  `docs/jev-shadow-evaluation.md:30`; Mercaria `packages/backend/.env.example:419-420`;
  Inbox README.

## Consecuencias

- **Los terceros que usan `api.oxy.so/v1` se rompen sin aviso** (documentado
  hoy en `O:docs/inference/sdk.md:181-193,260-280` y en las docs de
  Console). Aceptado por el dueño: deben cambiar a `https://kaana.ai/v1`; su
  clave `oxy_sk_` sigue valiendo.
- Kaana pasa a ser un servicio expuesto a clientes; `desired_count = 1` deja
  de ser aceptable.
- El prompt y la salida dejan de cruzar Oxy (Oxy ADR 0016 por
  construcción); Oxy ya no puede inspeccionar contenido para su política.
- Oxy conserva toda la autoridad económica y toda la gestión: sin concesión
  no hay ejecución, y la liquidación sale del recibo medido por Kaana.
- Kaana gana una clave privada (firma de liquidaciones), una bandeja de
  salida y la pertenencia a niveles revisada en git.
- El orden de financiación cambia en un punto: `discounted_payg` deja de ir
  antes que el crédito promocional (ADR 0001 decisión 3).
- Un fallo de `authorizations` bloquea toda la inferencia, como hoy un fallo
  del borde. No hay modo degradado sin concesión.
- `speed` y demás límites de voz dejan de fallar después de reservar.

## Riesgos

1. **Ventana del corte.** Entre la fase 3 y la 5 existen dos caminos en
   producción aunque ningún cliente use los dos; el borde viejo debe borrarse
   el mismo día.
2. **Aprobaciones Jev y Private Auto.** Fijan deployment, proveedor y
   `priceVersionId` y hoy hashean la entrada en Oxy
   (`inferenceEdge.service.ts:1940-1948`); deben reemitirse con el hash
   canónico de `@kaana.ai/contracts` y revisarse.
3. **Sesión de Inbox.** El pagador no cambia solo si la sesión del usuario
   resuelve a la app Inbox; si las sesiones de Inbox no llevan ese
   `clientId`, Inbox migra con su credencial de servicio y `onBehalfOfUserId`
   (mismo pagador, carril `service_token`).
4. **Intercambio de token para agentes de producto.** Es código nuevo de
   autenticación en Oxy; sin él, el carril de agentes de producto no puede
   cumplir la audiencia `kaana.ai`.
5. **Escala y abuso.** Kaana lee hasta 16 MB antes de verificar
   (`I:app-kaana.tf:483-489`); las reglas Cloudflare por path exigen plan
   Business o superior (runbook 38 de oxy-infra).
6. **Latencia de autorización.** `authorizations` es síncrona y sin caché
   (como ADR 0035 exige para `oxy_sk_`); un p99 alto de Oxy se suma al TTFB.
7. **Sesiones y CORS en el navegador.** Abrir `/v1/*` a orígenes de apps
   pone la inferencia al alcance de cualquier app registrada; la política y
   los presupuestos de Console deben estar configurados antes.
8. **Primera publicación en npm.** Si npm no permite configurar OIDC antes
   de que exista el paquete, hace falta una publicación inicial con token de
   corta vida.
9. **Bundle de Expo y navegador.** Si el SDK valida con zod en tiempo de
   ejecución, el playground y cualquier app con sesión cargan zod y el
   contrato.
10. **Paridad de niveles.** `power-classes.json` debe exportarse de la base de
    datos de producción de Oxy en el corte; una clase cambiada por migración
    revisada después de la semilla 0129 que no se exporte cambiaría niveles
    sin revisión.
