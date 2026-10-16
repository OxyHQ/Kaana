# ADR 0003 — Una API canónica: ninguna variante de proveedor llega al cliente

- Status: accepted (decisión del dueño, 2026-10-16)
- Date: 2026-10-16
- Decided by: el dueño de Oxy («Kaana es un proveedor de inferencia con su
  propia API, no un proxy»). Este documento la desarrolla, no la reabre.
- Enmienda: Kaana ADR 0002 en lo que publicaba el conjunto de contrato 5.0.0
  (`capabilities.realtime.parameters`, `audio.detectsLanguage`,
  `audio.transcriptionPrompt`): esas tablas pasan a ser internas.
- Contrato: conjunto 6.0.0 (`@kaana.ai/contracts` 6.0.0, `@kaana.ai/sdk` 3.0.0).

## Contexto

El conjunto 5.0.0 publicaba, por modelo, lo que la sesión realtime de su
proveedor aceptaba: qué detecciones de turno, qué rango de umbral, qué
modalidades de salida, qué `toolChoice`, si había tope de tokens, si la
transcripción aceptaba `prompt`, qué etiquetas de idioma exactas. Era honesto,
pero trasladaba al cliente las rarezas del cable de cada proveedor: un
cliente tenía que saber que xAI no tiene `semantic_vad`, que exige `es-MX` y
no `es`, que no acepta texto sin audio, que manda varios
`transcript.done` crecientes por turno… y escribir código por proveedor para
esquivarlo. Alia lo hizo el mismo día (consolidación de `transcript.done`
con un temporizador de 8 s, estimación de coste en unidades de xAI, elección
de idioma «que cubra», `toolChoice` explícito).

Eso es lo contrario de un proveedor de inferencia. OpenAI no publica las
diferencias entre sus centros de datos; Kaana no debe publicar las
diferencias entre sus proveedores.

## Decisión

**Kaana tiene una sola API, la suya. El cliente habla solo «Kaana»; Kaana
resuelve por dentro las diferencias entre xAI, OpenAI, Groq, Anthropic y
cualquier otro. Ningún cliente tiene código ni decisiones por proveedor.**

1. **El catálogo publica solo capacidades de producto:** modalidades y si hay
   realtime (`capabilities.realtime`: transportes, tipos de sesión,
   transcripción de entrada, codificaciones), idiomas, voces con género,
   formatos de audio de entrada y salida del contrato, precio (`pricing` y
   `pricePerMinute`) y límites del contrato. Nada del cable de un proveedor.

2. **La tabla por dialecto es interna.** `providerconfig.RealtimeStatement`
   (`contract.RealtimeParameters`: detecciones, rangos, modalidades,
   `toolChoice`, tope de tokens, prompt, unidades medidas, variantes de
   idioma por defecto) y `providerconfig.TranscriptionStatement` viven en el
   inventario de Kaana y dirigen tres cosas: la traducción, la emulación y el
   rechazo de lo imposible. No se publican.

3. **Kaana acepta todo lo que ofrece su contrato y lo traduce o lo emula:**
   - `toolChoice`: `auto` es el comportamiento por defecto y no se envía donde
     el cable no tiene campo; `none` en la sesión se emula retirando las
     tools (se envía `tools: []`); `required` o una función, donde el cable
     los tenga.
   - `outputModalities`: si el cable no admite lo pedido, Kaana pide audio,
     mide ese audio (se factura igual) y entrega lo pedido: sin audio y con la
     transcripción como `text.delta` cuando se pidió solo texto; con la
     transcripción también como texto cuando se pidieron los dos.
   - `maxOutputTokens`: se recorta al tope del cable.
   - Detección de turno: `semantic_vad` corre como el `server_vad` del
     proveedor si no lo tiene; el umbral del contrato (0–1) se proyecta
     linealmente sobre el rango del proveedor (xAI 0.1–0.9). Un `server_vad`
     sin `silenceDurationMs` espera el valor por defecto de KAANA,
     `REALTIME_DEFAULT_SILENCE_DURATION_MS` = 500 ms, en todos los
     proveedores, nunca el del proveedor. Medido en
     `x-ai/grok-voice-3.0` (2026-10-16), con una frase con pausas de 0,56 s y
     0,79 s:

     | Silencio | Respuestas | Canceladas | Latencia |
     |---|---|---|---|
     | el de xAI | 3 | 2 | empieza a hablar 39 ms antes de acabar la frase |
     | 500 ms | 1 | 0 | 490–576 ms |
     | 800 ms | 1 | 0 | 762 ms |
     | 1000 ms | 1 | 0 | 1031 ms |

     xAI respeta `silence_duration_ms` aunque su esquema no lo documente.
   - Idiomas: el cliente envía su propia etiqueta BCP 47 (`es-AR`). Kaana
     envía la grafía del proveedor (`providerconfig.NormalizeLanguage`:
     exacta, el idioma solo, la variante por defecto del proveedor, la
     primera variante) o ninguna, y el modelo detecta. Nunca se rechaza.
   - `prompt` de transcripción: es una pista; se envía donde el modelo la
     acepta y se descarta donde no.

4. **Solo lo imposible se rechaza**, antes del grant y sin nombrar al
   proveedor: `invalid_request` con el `param` del contrato y un mensaje
   canónico («this model always answers a turn it detected», «this model
   cannot call one named tool»…). `contract.RealtimeParameters.Unservable` es
   la única regla, y la aplican igual la API pública (antes del grant) y el
   dialecto (antes de escribir en el cable). Hoy solo xAI tiene casos:
   `createResponse`/`interruptResponse` en `false` con `server_vad`,
   `toolChoice` `required` o con función, `none` en un solo `response.create`
   (una respuesta no puede retirar las tools de la sesión), y el paso a
   push-to-talk de una sesión facturada por reloj.

   El código es `invalid_request`, no uno nuevo `unsupported_capability`:
   la lista de códigos públicos es cerrada y se parsea estricta, así que un
   código nuevo rompería a todo cliente ya desplegado justo en el error que
   tiene que entender. `invalid_request` + `param` ya dice qué campo y por
   qué.

5. **Lo que ningún proveedor sirve y no se puede emular sale del contrato**
   (ya en 5.0.0: `temperature` realtime, `strict` de las tools,
   `limits.maxTextItems`, `timestamps`/`segments`/`words`).

6. **Eventos normalizados.**
   - Exactamente UN `transcript.done` de entrada por item, el final: el primer
     transcript completo tras el commit del item. Los anteriores son
     transcripción en curso y solo producen deltas que añaden texto; los
     repetidos posteriores no producen nada. Un item ya confirmado cuyo final
     no llega recibe el texto que tiene cuando se confirma el siguiente, antes
     de ese commit. La concatenación de los deltas es el final siempre que el
     proveedor solo añada texto.
   - Ciclo de vida de respuestas completo: todo `response.created` tiene su
     `response.done` (Kaana#195).
   - Orden garantizado: el que el proveedor emite, con los eventos sintéticos
     de Kaana en el punto en que son ciertos.

7. **Uso y coste en unidades e importe de Kaana.** Cada modelo de voz publica
   `pricePerMinute`, el precio de Kaana por un minuto de su trabajo: un
   minuto de conversación con `server_vad` (la detección por defecto de
   Kaana) para un modelo realtime, un minuto de audio para uno de
   transcripción; redondeado hacia arriba, para reservar o mostrar sin
   conocer las unidades del proveedor. Lo que una petición cuesta es lo que
   midió, en `pricing`.

8. **Transcripción y speech igual:** un único contrato. Kaana adapta
   formatos, idiomas y opciones por proveedor (Groq Whisper, xAI, OpenAI…);
   solo se rechaza un formato de subida que el modelo no lee.

9. **`@kaana.ai/sdk/testing` implementa el contrato canónico**, no dialectos:
   acepta lo que acepta el contrato y rechaza solo lo que el catálogo dice que
   un modelo no tiene (voz, codificación, tipo de sesión, transporte, formato).

## Consecuencias

- Un cliente no lee nada por proveedor y no tiene ramas por proveedor. Elige
  modelo por capacidades de producto y precio por minuto.
- Añadir un proveedor es trabajo solo de Kaana: su declaración interna, su
  dialecto y sus pruebas (`TestTheDialectTranslatesEveryContractValue`
  cruza cada valor del contrato con cada dialecto: o se escribe en el cable,
  traducido o emulado, o `Unservable` lo rechaza con su `param`; nada en
  medio).
- Un cliente en 5.0.0 parsea todo lo que sirve 6.0.0 (solo desaparecen campos
  opcionales y aparece `pricePerMinute` en un objeto no estricto).
- Lo que Alia debe quitar: la consolidación de `transcript.done` por
  `input_audio.committed` con temporizador, la estimación de coste en unidades
  de xAI, la elección de idioma y región que «cubra», el `toolChoice`
  explícito y cualquier lectura de `capabilities.realtime.parameters`,
  `audio.detectsLanguage` o `audio.transcriptionPrompt`.
