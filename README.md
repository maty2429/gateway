# API gateway

Public clients use REST/JSON on `:8080`. Auth and admin routes under
`/api/v1/auth/*` and `/api/v1/admin/*` are translated to the versioned gRPC
contract exposed by auth. `/.well-known/jwks.json` serves the cached public key
set received from auth.

The gateway validates JWT signature, `kid`, issuer, expiry and audience before
forwarding protected requests. Auth then revalidates revocation and permissions.
The gateway never stores or generates an auth private key.

Mobile inicia sesión directamente mediante
`POST /api/v1/auth/mobile/login` y recibe todos sus proyectos y roles. Web
comienza en `GET /api/v1/auth/authorize`; el gateway conserva redirects 302,
cookie SSO y metadata segura a través de gRPC. El backend del proyecto canjea
el código en `POST /api/v1/auth/token`. Las operaciones de autorización y las
mutaciones nunca se reintentan.

For production, set `server.environment: production`, configure an explicit
CORS allowlist, set `auth.cookie_secure: true`, and configure the auth upstream:

```yaml
tls:
  ca_file: /run/secrets/internal-ca.crt
  cert_file: /run/secrets/gateway.crt
  key_file: /run/secrets/gateway.key
  server_name: auth.internal
```
