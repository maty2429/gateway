# Guía completa: API Gateway profesional en Go (mínimas dependencias)

> Objetivo: construir un API Gateway que reciba tráfico HTTP de clientes externos
> y lo enrute a microservicios internos vía HTTP o gRPC, con seguridad, resiliencia
> y observabilidad de nivel industria, usando la librería estándar de Go al máximo.

---

## 1. Visión general de la arquitectura

```
                    Internet (HTTPS)
                          │
                          ▼
                 ┌─────────────────┐
                 │  Load Balancer  │  (TLS termination opcional aquí)
                 └────────┬────────┘
                          │
            ┌─────────────┴─────────────┐
            ▼                           ▼
   ┌─────────────────┐        ┌─────────────────┐
   │  API Gateway #1 │        │  API Gateway #2 │   (stateless, escalable)
   └────────┬────────┘        └────────┬────────┘
            │                          │
   ─────────┴──── Red privada ────────┴─────────  (microservicios NO expuestos)
            │              │                │
            ▼              ▼                ▼
      ┌──────────┐   ┌──────────┐    ┌──────────┐
      │ Users MS │   │ Orders MS│    │ Pay MS   │
      │  (gRPC)  │   │  (gRPC)  │    │  (HTTP)  │
      └──────────┘   └──────────┘    └──────────┘
            │              │                │
            ▼              ▼                ▼
         (cada microservicio con su propia BD)

   Estado compartido del gateway (si hay múltiples réplicas):
      Redis → rate limiting distribuido, cache
```

### Principios fundamentales

1. **El gateway NO contiene lógica de negocio.** Solo enruta, protege, traduce y observa.
2. **Stateless.** Ningún estado en memoria que impida escalar horizontalmente.
3. **Único punto de exposición.** Los microservicios viven en red privada; solo el gateway (o el LB) mira a internet.
4. **Fail fast.** Timeouts en todo, circuit breakers, nunca requests colgados.
5. **Zero trust interno (ideal).** Los microservicios también validan identidad; el gateway no es la única barrera.

---

## 2. Responsabilidades del gateway

| Responsabilidad | Dónde se implementa | Dependencia |
|---|---|---|
| Routing / reverse proxy | `net/http` (router 1.22+), `httputil.ReverseProxy` | stdlib |
| Traducción HTTP→gRPC | Clientes gRPC + mapeo JSON↔proto | `google.golang.org/grpc` |
| Autenticación (JWT) | Middleware de auth | `golang-jwt/jwt` (pequeña) |
| Rate limiting | Middleware token bucket | `golang.org/x/time/rate` |
| Timeouts / cancelación | `context`, config del `http.Server` | stdlib |
| Circuit breaker | Implementación propia o `sony/gobreaker` | opcional |
| Logging estructurado | `log/slog` | stdlib |
| Métricas | `/metrics` Prometheus | `prometheus/client_golang` |
| Request ID / tracing | Middleware + propagación de headers | stdlib (`crypto/rand`) |
| CORS y headers de seguridad | Middleware | stdlib |
| Health checks | Handlers `/healthz`, `/readyz` | stdlib |
| Graceful shutdown | `signal.NotifyContext` + `server.Shutdown` | stdlib |

Total de dependencias directas: **4–5**. Todo lo demás es stdlib.

---

## 3. Patrones de diseño aplicados

### 3.1 Chain of Responsibility (cadena de middlewares)
El patrón central del gateway. Cada preocupación transversal es un middleware
`func(http.Handler) http.Handler` que envuelve al siguiente. El orden importa:

```
Request entrante
  → Recovery        (captura panics, responde 500 limpio)
  → Request ID      (genera/propaga X-Request-ID)
  → Logging         (log estructurado de cada request)
  → Métricas        (contador + histograma de latencia)
  → Security headers(HSTS, nosniff, etc.)
  → CORS            (preflight y orígenes permitidos)
  → Body size limit (MaxBytesReader)
  → Rate limiting   (por IP y/o por usuario)
  → Autenticación   (valida JWT, inyecta identidad en context)
  → Timeout         (context.WithTimeout por request)
  → Handler final   (proxy HTTP o cliente gRPC)
```

Regla: lo barato y universal primero (recovery, logging), lo caro y selectivo
después (auth). Rate limit **antes** de auth para que un ataque de fuerza bruta
no te haga validar miles de firmas JWT.

### 3.2 Facade
El gateway entero es una fachada: expone una API pública simple y esconde la
complejidad de N microservicios, sus protocolos y su topología.

### 3.3 Adapter (traducción de protocolo)
El módulo HTTP→gRPC es un adapter clásico: convierte requests JSON/HTTP en
llamadas gRPC tipadas y traduce las respuestas y errores de vuelta.
- Mapeo de códigos gRPC → HTTP: `NotFound`→404, `InvalidArgument`→400,
  `Unauthenticated`→401, `PermissionDenied`→403, `Unavailable`→503,
  `DeadlineExceeded`→504, `Internal`→500.

### 3.4 Circuit Breaker
Tres estados: **cerrado** (tráfico normal), **abierto** (upstream caído, se
rechaza rápido con 503), **semiabierto** (deja pasar requests de prueba).
Un breaker **por servicio upstream**, no global.

### 3.5 Retry con backoff exponencial + jitter
Solo para operaciones idempotentes (GET, HEAD). Máximo 2–3 reintentos.
Jitter aleatorio para evitar "retry storms" sincronizados.

### 3.6 Bulkhead (aislamiento)
Limitar conexiones/concurrencia por upstream para que un servicio lento no
consuma todos los recursos del gateway. En Go: semáforos (channel con buffer)
por servicio, o `MaxIdleConnsPerHost` en el transport.

### 3.7 Strategy (routing configurable)
Las rutas y sus upstreams se definen en configuración (YAML), no en código.
Cada ruta declara: método, path, servicio destino, protocolo (http/grpc),
si requiere auth, su timeout y su rate limit.

### 3.8 Singleton controlado (conexiones)
Un `grpc.ClientConn` por microservicio, creado en el arranque y reutilizado
(las conexiones gRPC son multiplexadas y thread-safe). Jamás crear conexión
por request. Igual con `http.Client`: uno por upstream, con transport afinado.

### 3.9 Decorator
Los middlewares son decorators sobre `http.Handler`. También aplica a los
clientes: puedes decorar un cliente gRPC con breaker + retry + métricas.

---

## 4. Estructura de proyecto recomendada

```
api-gateway/
├── cmd/
│   └── gateway/
│       └── main.go              # arranque: config, wiring, servidor, shutdown
├── internal/
│   ├── config/
│   │   └── config.go            # carga YAML + env vars, validación
│   ├── middleware/
│   │   ├── recovery.go
│   │   ├── requestid.go
│   │   ├── logging.go
│   │   ├── metrics.go
│   │   ├── security.go          # headers de seguridad
│   │   ├── cors.go
│   │   ├── ratelimit.go
│   │   ├── auth.go              # validación JWT
│   │   └── timeout.go
│   ├── proxy/
│   │   ├── http.go              # reverse proxy HTTP (httputil)
│   │   └── grpc.go              # adapter HTTP→gRPC + mapeo de errores
│   ├── resilience/
│   │   ├── breaker.go           # circuit breaker
│   │   └── retry.go
│   ├── router/
│   │   └── router.go            # construye el mux desde la config
│   └── health/
│       └── health.go            # /healthz, /readyz
├── proto/                       # .proto compartidos (o módulo aparte)
├── configs/
│   ├── gateway.yaml             # rutas, upstreams, límites
│   └── gateway.prod.yaml
├── deployments/
│   ├── Dockerfile               # multi-stage, imagen distroless/scratch
│   └── k8s/                     # manifests si usas Kubernetes
├── go.mod
└── Makefile                     # build, test, lint, proto-gen
```

`internal/` garantiza que nadie importe tus paquetes desde fuera del módulo.

### Ejemplo de configuración (gateway.yaml)

```yaml
server:
  addr: ":8080"
  read_timeout: 10s
  read_header_timeout: 5s
  write_timeout: 30s
  idle_timeout: 120s
  max_body_bytes: 1048576        # 1 MiB

auth:
  jwt_issuer: "https://auth.miapp.com"
  jwt_audience: "api.miapp.com"
  # claves públicas vía JWKS o archivo; NUNCA la clave en el yaml

rate_limit:
  default_rps: 20
  default_burst: 40

upstreams:
  users:
    protocol: grpc
    address: "users-svc:50051"
    timeout: 3s
  orders:
    protocol: grpc
    address: "orders-svc:50051"
    timeout: 5s
  payments:
    protocol: http
    address: "http://payments-svc:8080"
    timeout: 8s

routes:
  - method: GET
    path: /api/v1/users/{id}
    upstream: users
    auth: required
  - method: POST
    path: /api/v1/auth/login
    upstream: users
    auth: none
    rate_limit: { rps: 3, burst: 5 }    # login más estricto
  - method: POST
    path: /api/v1/orders
    upstream: orders
    auth: required
    idempotency: true
```

---

## 5. Paso a paso de construcción (por fases)

### Fase 0 — Preparación
1. `go mod init github.com/tuusuario/api-gateway` (Go 1.22+ mínimo, por el router).
2. Crear estructura de carpetas de la sección 4.
3. Makefile con targets: `build`, `run`, `test`, `lint` (usa `go vet` + `staticcheck`).
4. Definir los `.proto` de tus microservicios y generar el código Go
   (`protoc` con `protoc-gen-go` y `protoc-gen-go-grpc`).

### Fase 1 — Esqueleto HTTP sólido
1. `http.Server` con **todos** los timeouts configurados (Go no trae ninguno por defecto):
   `ReadTimeout`, `ReadHeaderTimeout`, `WriteTimeout`, `IdleTimeout`.
2. Router con `http.NewServeMux()` usando patrones `"GET /api/v1/users/{id}"`.
3. Handlers `/healthz` (proceso vivo) y `/readyz` (dependencias listas).
4. **Graceful shutdown**: `signal.NotifyContext(ctx, SIGINT, SIGTERM)` →
   `server.Shutdown(ctx)` con timeout de gracia (ej. 15s).
5. Formato de error uniforme desde el día 1 (RFC 7807): `{"type","title","status","detail","instance"}`.
   Ningún error interno se filtra al cliente.

✅ Criterio de salida: el servidor arranca, responde health checks, se apaga limpio.

### Fase 2 — Cadena de middlewares base
1. **Recovery**: `defer recover()`, loguea el panic con stack, responde 500 genérico.
2. **Request ID**: genera un ID (UUID v4 con `crypto/rand`), lo pone en el
   `context`, en el header de respuesta y lo propagarás a los upstreams.
3. **Logging** con `log/slog` en JSON: método, ruta, status, latencia, bytes,
   IP, request_id, user_id (si hay). Nunca loguear tokens, passwords ni PII.
4. **Métricas**: contador de requests por ruta/status e histograma de latencia.
   Exponer `/metrics` en un puerto interno separado (no público).
5. Helper para componer: `Chain(h, mw1, mw2, ...)`.

✅ Criterio: cada request produce un log JSON con request_id y métricas.

### Fase 3 — Reverse proxy HTTP
1. `httputil.NewSingleHostReverseProxy` (o `ReverseProxy` con `Rewrite`) por upstream HTTP.
2. Afinar el `http.Transport`: `MaxIdleConns`, `MaxIdleConnsPerHost`,
   `IdleConnTimeout`, `TLSHandshakeTimeout`.
3. En el `Rewrite/Director`: setear `X-Forwarded-For`, `X-Request-ID`,
   y **eliminar headers internos entrantes** (`X-User-ID`, etc.) para que
   un cliente no pueda inyectarlos.
4. `ErrorHandler` del proxy: traducir fallos de red a 502/504 en formato RFC 7807.
5. Timeout por request con `context.WithTimeout` según la config de la ruta.

✅ Criterio: puedes proxear a un microservicio HTTP de prueba con timeouts reales.

### Fase 4 — Adapter gRPC
1. En el arranque, crear un `grpc.ClientConn` por servicio gRPC (con
   `grpc.WithDefaultServiceConfig` para keepalive/balanceo) y reutilizarlo.
2. Por cada endpoint: decodificar JSON → struct proto (valida tamaño y
   content-type antes), llamar al método gRPC con el context del request
   (así se propaga el timeout y la cancelación), codificar respuesta → JSON.
3. Propagar `X-Request-ID` y la identidad del usuario vía **gRPC metadata**.
4. Mapear `status.Code(err)` a códigos HTTP (tabla de la sección 3.3).
5. Cerrar las conexiones en el shutdown.

✅ Criterio: un endpoint público JSON funciona contra un microservicio gRPC.

### Fase 5 — Autenticación
1. Middleware JWT: extraer `Authorization: Bearer <token>`, validar firma
   (RS256/EdDSA con clave pública — evita HS256 compartido entre servicios),
   `exp`, `iat`, `iss`, `aud`.
2. Soportar `kid` en el header del JWT y múltiples claves activas (rotación
   sin downtime). Ideal: consumir un endpoint JWKS del servicio de auth con cache.
3. Inyectar los claims en el `context` para middlewares/handlers posteriores.
4. Autorización **gruesa** en el gateway (¿token válido?, ¿rol mínimo?);
   la fina (ownership del recurso) vive en cada microservicio.
5. Rutas públicas declaradas explícitamente en config (`auth: none`);
   el default debe ser **auth requerida** (fail-closed).
6. Access tokens cortos (5–15 min) + refresh tokens manejados por tu servicio
   de auth (el gateway no emite tokens, solo valida).

✅ Criterio: rutas protegidas devuelven 401 sin token válido; los upstreams
reciben la identidad de forma confiable.

### Fase 6 — Rate limiting
1. `golang.org/x/time/rate`: un limiter por clave (IP para anónimos,
   user_id/API key para autenticados), guardados en un map con mutex y
   limpieza periódica de entradas viejas (evitar fuga de memoria).
2. Límites por ruta desde la config; login/registro mucho más estrictos.
3. Responder 429 con header `Retry-After`.
4. Si escalas a varias réplicas: mover a Redis (sliding window / GCRA).
   Diseña la interfaz `RateLimiter` desde ahora para poder cambiar la
   implementación sin tocar el middleware.

✅ Criterio: superar el límite produce 429; el límite de login es independiente.

### Fase 7 — Resiliencia
1. **Circuit breaker por upstream**: umbral de fallos (ej. 5 consecutivos o
   >50% en ventana), estado abierto por N segundos, semiabierto con requests
   de prueba. Al estar abierto: 503 inmediato + métrica.
2. **Retries** con backoff exponencial + jitter, solo GET/HEAD, máximo 2.
3. **Bulkhead**: semáforo de concurrencia por upstream.
4. Verificar propagación de cancelación: si el cliente aborta, la llamada
   al microservicio debe cancelarse (esto sale gratis si usas `r.Context()`
   en toda la cadena).

✅ Criterio: apagar un microservicio no degrada al resto; el gateway responde
503 rápido en vez de colgarse.

### Fase 8 — Endurecimiento de seguridad
1. TLS: termina en el LB o en el gateway (`http.ListenAndServeTLS` con
   `tls.Config{MinVersion: tls.VersionTLS12}`). Redirigir HTTP→HTTPS.
2. Headers de respuesta: `Strict-Transport-Security`,
   `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`,
   `Referrer-Policy`, y eliminar `Server`.
3. CORS con whitelist explícita de orígenes; nunca `*` con credenciales.
4. `http.MaxBytesReader` para limitar el body (configurable por ruta).
5. Sanitizar headers entrantes: borrar todo header interno reservado.
6. Secretos solo por env vars / secret manager. Nada en el repo.
7. mTLS interno gateway↔microservicios si el entorno lo permite; como mínimo,
   red privada donde los microservicios no sean alcanzables desde internet.
8. `govulncheck` en CI para detectar dependencias vulnerables.

### Fase 9 — Observabilidad completa
1. Dashboard con las 4 señales doradas: tráfico, errores, latencia (p50/p95/p99),
   saturación — por ruta y por upstream.
2. Alertas: tasa de 5xx, breaker abierto, p99 sobre umbral, `/readyz` fallando.
3. (Opcional) OpenTelemetry para tracing distribuido; si no, el request_id
   propagado ya te da correlación de logs entre servicios.

### Fase 10 — Testing y despliegue
1. **Unit tests** de cada middleware con `net/http/httptest`.
2. **Integración**: gateway + microservicio fake (servidor gRPC en memoria
   con `bufconn`) verificando el flujo completo, incluido el mapeo de errores.
3. **Carga**: `k6` o `vegeta`; medir p99 y encontrar el punto de saturación
   ANTES de producción. Probar también el comportamiento con un upstream lento.
4. **Docker**: build multi-stage → binario estático en imagen `distroless`
   o `scratch`, usuario no-root, `HEALTHCHECK`.
5. Réplicas ≥2 detrás de un load balancer; el gateway es stateless así que
   escalar es agregar pods/instancias.
6. Versionado de API (`/api/v1/`) y política de deprecación documentada.

---

## 6. Checklist final de calidad "nivel industria"

- [ ] Ningún timeout en infinito (servidor, clientes, contextos)
- [ ] Graceful shutdown probado (SIGTERM no corta requests en vuelo)
- [ ] Errores uniformes RFC 7807; cero stack traces al cliente
- [ ] Request ID en todos los logs y propagado a upstreams
- [ ] Headers internos imposibles de inyectar desde fuera
- [ ] Default fail-closed: ruta nueva = auth requerida salvo que se declare pública
- [ ] Rate limit distinto para endpoints sensibles (login, registro, reset password)
- [ ] Circuit breaker por upstream con métrica de estado
- [ ] Conexiones gRPC/HTTP reutilizadas (creadas una vez)
- [ ] Config externa (YAML + env), sin recompilar para agregar rutas
- [ ] Secretos fuera del código y del repo
- [ ] Logs sin PII ni tokens
- [ ] Métricas y alertas antes del primer despliegue
- [ ] Pruebas de carga hechas; capacidad conocida
- [ ] Imagen mínima, non-root, con health checks
- [ ] `go vet`, `staticcheck` y `govulncheck` en CI

---

## 7. Errores comunes a evitar

1. **Meter lógica de negocio en el gateway** → se convierte en un monolito distribuido.
2. **Crear conexión gRPC por request** → latencia y agotamiento de recursos.
3. **Olvidar los timeouts del `http.Server`** → vulnerable a Slowloris.
4. **Confiar en headers del cliente** para identidad → suplantación trivial.
5. **Retries en POST no idempotentes** → operaciones duplicadas (pagos dobles).
6. **Rate limit solo por IP** → inútil contra atacantes con muchas IPs y castiga NATs corporativos; combina IP + identidad.
7. **`*` en CORS con credenciales** → los navegadores lo bloquean o abres un agujero.
8. **Loguear el body de requests** → PII y tokens en tus logs.
9. **Un solo circuit breaker global** → un servicio caído tumba todos los demás.
10. **Exponer `/metrics` o `/debug/pprof` al público** → información sensible; puerto interno.

---

## 8. Ruta de aprendizaje sugerida

1. Fases 1–3 primero (esqueleto + middlewares + proxy HTTP): con eso ya tienes
   un gateway funcional y entiendes el 70% del patrón.
2. Fase 4 (gRPC) cuando el flujo HTTP esté sólido.
3. Fases 5–7 (auth, rate limit, resiliencia) antes de exponer nada a internet.
4. Fases 8–10 como endurecimiento continuo.

Referencias que la industria usa como base: los docs de `net/http` y
`httputil.ReverseProxy`, gRPC-Go (guías de "performance best practices"),
OWASP API Security Top 10, y RFC 7807 para errores.
