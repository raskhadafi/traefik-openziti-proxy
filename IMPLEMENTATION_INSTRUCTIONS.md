# Implementation instructions: Traefik OpenZiti proxy (plugin + sidecar)

These instructions are written for the coding model. Build exactly what is
described here, in the order of section 9.

## 1. Goal

Turn Traefik into a public ingress for an OpenZiti overlay network. Two parts
live in this repository:

- **Plugin**: a small, dependency-free Traefik middleware. It forwards each
  request to the sidecar and tells it which Ziti service to use.
- **Sidecar**: a standalone Go binary that holds the Ziti identity. It
  completes enrollment, connects to the Ziti network and dials services.

```
browser (no Ziti software)
   │  https://test.ch:443
   ▼
Traefik entrypoint :443, router rule Host(`test.ch`), TLS terminated by Traefik
   │
   ▼
plugin (middleware)  ── plain HTTP, header X-Ziti-Service: my-service ──►  sidecar
                                                                             │ dials Ziti service
                                                                             ▼
                                                                      Ziti edge router
                                                                             │
                                            hosting identity / tunneler with host.v1 config
                                                                             │
                                                                             ▼
                                                             real destination (e.g. 10.0.0.5:8080)
```

Decisions already made by the user. Do not revisit them:

| Topic | Decision |
|---|---|
| Architecture | Plugin + sidecar. The OpenZiti SDK is used only in the sidecar. |
| Direction | The sidecar **dials** a Ziti service. Nothing binds/hosts one. |
| `test.ch:443` | Matched by Traefik itself (router rule + entrypoint). There is no hostname or port field anywhere in this project's config. |
| host.v1 | Lives on the hosting side of the Ziti service. This project does not read or create it. |
| Enrollment | JWT is read from a file. The enrolled identity is persisted to a file and reused on restart. |

Why the sidecar exists: the OpenZiti Go SDK requires Go 1.26 and pulls in about
315 third-party packages, some using unsafe system calls. Traefik's plugin
interpreter (Yaegi) cannot be relied on to load that. Consequence: the JWT and
identity file are configured on the sidecar, not in the Traefik plugin config.

The plugin is a terminal middleware: it answers the request itself and never
calls `next`. The Traefik router still needs a `service` to be valid, so
examples use `noop@internal`.

## 2. Verified facts about the OpenZiti SDK

Checked against `github.com/openziti/sdk-golang@latest` on 2026-11-11. Re-check
signatures with `go doc` against the version you pin.

- Its `go.mod` declares `go 1.26.0`.
- Enrollment:
  - `enroll.ParseToken(tokenStr string) (*ziti.EnrollmentClaims, *jwt.Token, error)`
  - `enroll.Enroll(enroll.EnrollmentFlags) (*ziti.Config, error)` with fields
    `Token`, `JwtToken`, `KeyAlg` (`ziti.KeyAlgVar`; call `.Set("EC")` or `.Set("RSA")`).
  - The returned `*ziti.Config` is JSON-serialisable; that JSON is the identity file.
- Loading and dialing:
  - `ziti.NewConfigFromFile(path) (*ziti.Config, error)`
  - `ziti.NewContext(cfg) (ziti.Context, error)`
  - `Context.DialContext(ctx, serviceName) (edge.Conn, error)`; `edge.Conn` is a `net.Conn`.
  - `Context.Authenticate() error`, `Context.GetServices()`, `Context.Close()`.

## 3. Repository layout

Two Go modules, so the plugin module stays free of dependencies:

```
.traefik.yml
go.mod                         // module github.com/raskhadafi/traefik-openziti-proxy, no requires
openziti.go                    // plugin: Config, CreateConfig, New, handler
openziti_test.go
sidecar/
  go.mod                       // module github.com/raskhadafi/traefik-openziti-proxy/sidecar
  go.sum
  main.go                      // flags, wiring, HTTP server, shutdown
  identity.go                  // loadOrEnroll
  proxy.go                     // handler, transport cache
  identity_test.go
  proxy_test.go
  Dockerfile
examples/
  traefik.yml                  // static config, local-plugin mode
  dynamic.yml                  // router + middleware
  docker-compose.yml           // Traefik + sidecar
.github/workflows/ci.yml
.gitignore
README.md
LICENSE                        // Apache-2.0
```

Root module rules: package name `traefik_openziti_proxy`, standard library
only, no `vendor/` directory, `go` directive no newer than what the Yaegi
version in the current Traefik v3 release supports (check its docs; if unsure
use `go 1.22`). Avoid generics and other recent language features in the plugin.

Sidecar module rules: do not vendor. `go` directive as required by the SDK.

## 4. Plugin to sidecar protocol

Plain HTTP/1.1. The plugin forwards the original request unchanged (method,
path, query, headers, body, upgrades) and adds control headers:

| Header | Required | Meaning |
|---|---|---|
| `X-Ziti-Service` | yes | Ziti service name to dial. |
| `X-Ziti-Upstream-Scheme` | no | `http` (default) or `https`: protocol spoken to the destination inside the Ziti connection. |
| `X-Ziti-Upstream-Host` | no | Overrides the Host header (and TLS SNI) sent to the destination. Default: the incoming Host. |

Rules:

- The plugin **deletes** every incoming header whose name starts with
  `X-Ziti-` before adding its own. A client must never be able to choose the service.
- The sidecar removes all `X-Ziti-*` headers before forwarding to the destination.
- The original `Host` header is preserved from plugin to sidecar.
- A sidecar request without `X-Ziti-Service`: `GET /healthz` returns `200 ok`
  when the Ziti context is authenticated and `503` otherwise; anything else
  returns `400`.

## 5. Plugin

### 5.1 Manifest `.traefik.yml`

```yaml
displayName: OpenZiti Proxy
type: middleware
import: github.com/raskhadafi/traefik-openziti-proxy
summary: Forwards HTTP requests into an OpenZiti network through a companion sidecar that dials a Ziti service.
testData:
  service: my-service
  sidecarAddress: http://127.0.0.1:8900
```

### 5.2 Config

```go
type Config struct {
    Service        string `json:"service"`        // required. Ziti service name.
    SidecarAddress string `json:"sidecarAddress"` // default "http://127.0.0.1:8900". Also accepts "unix:///path/to.sock".
    UpstreamScheme string `json:"upstreamScheme"` // "http" (default) or "https".
    UpstreamHost   string `json:"upstreamHost"`   // optional.
}
```

Exported API, exactly:

```go
func CreateConfig() *Config
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error)
```

`New` validates: `Service` non-empty; `UpstreamScheme` is `http` or `https`;
`SidecarAddress` parses as `http://host:port` or `unix:///absolute/path`.
Errors include the middleware `name`. `New` must not contact the sidecar; the
sidecar may start later than Traefik.

### 5.3 Handler

- One `http.Transport` per middleware instance. For `unix://` addresses set
  `DialContext` to dial the socket path and use `http://sidecar` as the target URL.
  `MaxIdleConnsPerHost: 32`, `IdleConnTimeout: 90s`.
- `httputil.ReverseProxy` targeting the sidecar. Keep the incoming `Host`
  header, apply the header rules of section 4, set `X-Forwarded-For`,
  `X-Forwarded-Proto` and `X-Forwarded-Host` (do not let the sidecar add them a second time).
  If `Rewrite` is not supported by the Yaegi version, use `Director`.
- `FlushInterval: -1`. WebSocket upgrades must pass through.
- `ErrorHandler`: sidecar unreachable gives `502 Bad Gateway` with a short
  plain-text body and one log line. Sidecar responses, including its own
  502/504, are passed through unchanged.
- `ServeHTTP` never calls `next`.

## 6. Sidecar

### 6.1 Configuration

Flags, each with an environment variable fallback (flag wins):

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--listen` | `ZITI_SIDECAR_LISTEN` | `127.0.0.1:8900` | `host:port` or `unix:///path/to.sock`. |
| `--identity-file` | `ZITI_IDENTITY_FILE` | required | Where the enrolled identity JSON is stored. |
| `--jwt-file` | `ZITI_JWT_FILE` | empty | One-time enrollment JWT. Needed only when the identity file does not exist yet. |
| `--key-alg` | `ZITI_KEY_ALG` | `EC` | `EC` or `RSA`. |
| `--allowed-services` | `ZITI_ALLOWED_SERVICES` | empty = all | Comma-separated allowlist of service names. Others get `403`. |
| `--dial-timeout` | `ZITI_DIAL_TIMEOUT` | `15s` | Go duration. |
| `--insecure-skip-verify` | `ZITI_INSECURE_SKIP_VERIFY` | `false` | Applies to `https` upstreams only. |

For a unix socket: remove a stale socket file on start, create it with mode `0660`.

### 6.2 Identity bootstrap

Function `loadOrEnroll` in `sidecar/identity.go`:

1. If the identity file exists and is non-empty: `ziti.NewConfigFromFile`. Ignore the JWT file.
2. Otherwise the JWT file must be set and readable; if not, exit with an error naming both paths.
3. Read the JWT, `strings.TrimSpace`, `enroll.ParseToken`, then `enroll.Enroll`
   with `Token`, `JwtToken`, `KeyAlg`.
4. Marshal the returned config to JSON. Write to `<identity-file>.tmp` with
   mode `0600`, then `os.Rename`. Create the parent directory with `0700` if missing.
5. If the write fails after a successful enrollment, the error must say that
   the JWT is now consumed and a new one must be issued. Do not delete the JWT file.
6. Never log the JWT or the identity contents.

Pass the enroll function in as a parameter so tests can replace it.

### 6.3 Startup and lifetime

1. Parse and validate config. Invalid config exits non-zero.
2. `loadOrEnroll`, `ziti.NewContext`, `Authenticate()`. Failure exits non-zero
   with a clear message; the container restart policy handles retries.
3. Log a warning for each service in `--allowed-services` that `GetServices()`
   does not show as dialable. Do not exit; policies can be granted later.
4. Serve HTTP. On SIGINT/SIGTERM: `http.Server.Shutdown` with a 10s timeout,
   then close the Ziti context.

### 6.4 Request handling

- Put the dialer behind an interface so tests need no Ziti network:
  ```go
  type dialer interface {
      DialContext(ctx context.Context, service string) (net.Conn, error)
  }
  ```
- Validate headers: missing service follows section 4; service not in the
  allowlist gives `403`; bad scheme gives `400`.
- Cache one `http.Transport` per (service, scheme, upstream host) in a
  mutex-guarded map. Its `DialContext` ignores network/address and calls
  `dialer.DialContext(ctx, service)` under the dial timeout. For `https` set
  `TLSClientConfig` with `ServerName` = upstream host (without port) and
  `InsecureSkipVerify` from the flag. `MaxIdleConnsPerHost: 32`, `IdleConnTimeout: 90s`.
- `httputil.ReverseProxy` with `Rewrite`: set `URL.Scheme`; set `URL.Host` and
  the outgoing `Host` to `X-Ziti-Upstream-Host` or the incoming Host; strip
  `X-Ziti-*`; keep the `X-Forwarded-*` headers that the plugin set.
- `FlushInterval: -1`. WebSocket upgrades must work.
- `ErrorHandler`: `504 Gateway Timeout` when the error is a context deadline,
  otherwise `502 Bad Gateway`. Short plain-text body without internal details.
  Log service name and error.

### 6.5 Logging

Standard library `log/slog`, text handler, to stderr. One line per event:
enrollment performed, identity loaded, authenticated, listening address,
service not dialable, each proxy error. No per-request access log.

### 6.6 Dockerfile

Multi-stage: `golang` image matching the SDK's Go version builds a static
binary (`CGO_ENABLED=0`); final stage is distroless static, non-root user,
`ENTRYPOINT` the binary. Build context is the `sidecar/` directory.

## 7. Examples and README

`examples/dynamic.yml` must show the user's scenario:

```yaml
http:
  routers:
    test-ch:
      rule: Host(`test.ch`)
      entryPoints: [websecure]        # :443
      tls: {}
      middlewares: [to-ziti]
      service: noop@internal
  middlewares:
    to-ziti:
      plugin:
        openziti:
          service: my-service
          sidecarAddress: http://ziti-sidecar:8900
```

`examples/traefik.yml`: entrypoints `web` and `websecure`, file provider, and

```yaml
experimental:
  localPlugins:
    openziti:
      moduleName: github.com/raskhadafi/traefik-openziti-proxy
```

`examples/docker-compose.yml`: Traefik (repo mounted at
`/plugins-local/src/github.com/raskhadafi/traefik-openziti-proxy`) and the
sidecar built from `sidecar/`, with `--listen 0.0.0.0:8900`, a named volume at
`/ziti` for the identity file, the JWT mounted read-only, and no published
port for the sidecar.

README sections: what it does (diagram from section 1); why there is a
sidecar; prerequisites on the Ziti side (identity created with a JWT, dial
service policy for that identity, service hosted with a host.v1 config, edge
router policies); quick start with the compose example; plugin config
reference; sidecar flag reference; enrollment lifecycle (JWT is one-time; the
identity file must be on a persistent volume; to re-enroll delete the identity
file and supply a new JWT); security notes (keep the sidecar listener private,
use `--allowed-services`, the identity file is a credential); troubleshooting.

`.gitignore`: `*.jwt`, `identity*.json`, the sidecar binary.

CI (`.github/workflows/ci.yml`): for both modules run `gofmt -l` (must be
empty), `go vet ./...`, `go test -race ./...`; build the sidecar Docker image.

## 8. Tests and acceptance

Plugin (`openziti_test.go`), against an `httptest.Server` acting as sidecar:

- Config validation: each invalid field produces an error.
- Control headers are set from config; client-supplied `X-Ziti-*` headers are replaced or removed.
- Host header, method, path, query and body arrive unchanged; `X-Forwarded-*` are set.
- Unix-socket sidecar address works.
- Sidecar down gives 502; a sidecar 504 is passed through.
- `next` is never called.

Sidecar, with a fake `dialer` connecting to an `httptest.Server`:

- `loadOrEnroll`: existing identity file is used and the JWT is not read;
  missing identity and missing JWT gives a clear error; successful enrollment
  leaves no `.tmp` file and the result has mode `0600`.
- GET and POST round-trip; `X-Ziti-*` headers do not reach the destination.
- Host header with and without `X-Ziti-Upstream-Host`.
- Allowlist: unlisted service gives 403.
- Dial failure gives 502; dial timeout gives 504.
- WebSocket upgrade round-trips through plugin and sidecar together.
- `/healthz` behaviour.

Done means all of:

1. `gofmt`, `go vet ./...` and `go test -race ./...` pass in both modules.
2. The newest Traefik v3 release loads the plugin in local-plugin mode with no
   Yaegi errors in its log, verified by actually running it (Docker image, or
   the release binary if Docker is unavailable), and a request through Traefik
   reaches a stand-in sidecar.
3. If an OpenZiti network can be started in the environment (for example the
   OpenZiti quickstart container), run one real enrollment and one end-to-end
   request through Traefik, plugin and sidecar to a host.v1 destination, and
   put the result in the report. If that is not possible, say so explicitly
   and do not claim end-to-end verification.
4. Everything is committed and pushed to branch `claude/clever-ramanujan-1vbieo`.
   Do not open a pull request.

## 9. Order of work

1. Root module, `.traefik.yml`, plugin code and plugin tests.
2. Run Traefik with the plugin (acceptance item 2) before going further. If
   Yaegi rejects the plugin, fix the plugin code; that is the only part that
   must be interpreter-compatible.
3. Sidecar module: identity, proxy, main, tests.
4. Dockerfile, examples, CI, README, LICENSE.
5. End-to-end check (acceptance item 3), then commit, push and report.

## 10. Rules

- No secrets, real JWTs or identity files in the repo.
- No config fields, features or abstractions beyond this document.
- When a fact here conflicts with what the SDK source or the Traefik docs
  show, trust the source and mention the difference in the final report.
- Report honestly: failed or skipped steps are listed as such with their output.
