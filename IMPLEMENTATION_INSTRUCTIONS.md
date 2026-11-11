# Implementation instructions: Traefik OpenZiti proxy plugin

These instructions are written for the coding model. Follow the phases in order.
Phase 1 ends in a hard stop-and-report gate. Do not start Phase 2 unless the
gate passes.

## 1. Goal

A Traefik middleware plugin that turns Traefik into a public ingress for an
OpenZiti overlay network.

```
browser (no Ziti software)
   │  https://test.ch:443
   ▼
Traefik entrypoint :443, router rule Host(`test.ch`), TLS terminated by Traefik
   │
   ▼
this plugin (middleware)  ── dials OpenZiti service "my-service" ──►  Ziti edge router
                                                                          │
                                         hosting identity / tunneler with host.v1 config
                                                                          │
                                                                          ▼
                                                              real destination (e.g. 10.0.0.5:8080)
```

Decisions already made by the user. Do not revisit them:

| Topic | Decision |
|---|---|
| Direction | The plugin **dials** a Ziti service. It never binds/hosts one. |
| `test.ch:443` | Matched by Traefik itself (router rule + entrypoint). The plugin config has **no** hostname or port field, only the Ziti service name. |
| host.v1 | Lives on the hosting side of the Ziti service. The plugin does not read or create it. |
| Enrollment | JWT is read from a file. The enrolled identity is persisted to a file and reused on restart. |
| Runtime | Try a native Yaegi plugin first (Phase 1). If that fails, stop and report. |

Terminal middleware: the plugin answers the request itself by reverse-proxying
over Ziti. It never calls `next`. The Traefik router still needs a `service`
entry to be valid, so examples use a dummy one (`noop@internal`).

## 2. Verified facts about the OpenZiti SDK

Checked against `github.com/openziti/sdk-golang@latest` on 2026-11-11. Re-check
signatures with `go doc` against the version you actually pin.

- Its `go.mod` declares `go 1.26.0`.
- Importing `ziti` + `ziti/enroll` pulls in about 315 non-stdlib packages,
  including `github.com/ebitengine/purego` (unsafe + syscall), protobuf and
  websocket libraries.
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

These facts make the Yaegi path unlikely to work (Yaegi lags behind the Go
toolchain and interprets every vendored dependency). That is why Phase 1 is a
spike and not the implementation.

## 3. Phase 1: Yaegi feasibility spike (STOP gate)

Purpose: prove or disprove that Traefik can load a plugin that imports the SDK.
Write the minimum; no tests, no polish.

1. Module path: `github.com/raskhadafi/traefik-openziti-proxy`, package name
   `traefik_openziti_proxy`, all plugin code in the repo root.
2. Create `.traefik.yml`:
   ```yaml
   displayName: OpenZiti Proxy
   type: middleware
   import: github.com/raskhadafi/traefik-openziti-proxy
   summary: Forwards HTTP requests into an OpenZiti network by dialing a Ziti service.
   testData:
     service: my-service
     jwtFile: /ziti/enroll.jwt
     identityFile: /ziti/identity.json
   ```
3. Create the spike plugin file exporting exactly the Traefik plugin API:
   ```go
   func CreateConfig() *Config
   func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error)
   ```
   In `New`, reference both `enroll.ParseToken` and `ziti.NewContext` so the
   interpreter must load both packages (call `ParseToken("")` and ignore the error).
4. `go mod tidy && go mod vendor`. Traefik plugins must ship their `vendor/`
   directory; commit it.
5. Run Traefik in local-plugin mode. Use the newest Traefik v3 release
   (Docker image, or the release binary if Docker is unavailable).
   - Mount the repo at `/plugins-local/src/github.com/raskhadafi/traefik-openziti-proxy`.
   - Static config:
     ```yaml
     experimental:
       localPlugins:
         openziti:
           moduleName: github.com/raskhadafi/traefik-openziti-proxy
           settings:
             useUnsafe: true
     ```
     Check the Traefik docs of that release for the exact name of the unsafe
     setting; if it does not exist in that release, record that as a finding.
   - Attach the middleware to one router via a file-provider dynamic config.
6. Read the Traefik startup log.

**Gate:**

- PASS = Traefik starts, the log shows the plugin loaded with no Yaegi
  error, and a request to the router reaches the plugin handler.
- FAIL = anything else (import errors, unsupported syntax, panics, plugin disabled).

If FAIL: try at most two cheap fixes (pin an older SDK release whose `go`
directive Yaegi supports; drop the `enroll` import to see whether `ziti` alone
loads). Then **stop**. Commit the spike, push, and report to the user:

- the exact Traefik version and SDK version(s) tried,
- the first Yaegi error lines verbatim,
- which of the fallbacks in section 7 you recommend.

Do not implement a fallback without the user's go-ahead.

## 4. Phase 2: implementation (only after the gate passes)

### 4.1 Config

```go
type Config struct {
    Service        string `json:"service"`        // required. Ziti service name to dial.
    JWTFile        string `json:"jwtFile"`        // path to the one-time enrollment JWT.
    IdentityFile   string `json:"identityFile"`   // required. Path where the enrolled identity JSON is stored.
    KeyAlg         string `json:"keyAlg"`         // "EC" (default) or "RSA".
    UpstreamScheme string `json:"upstreamScheme"` // "http" (default) or "https": protocol spoken to the destination inside the Ziti connection.
    UpstreamHost   string `json:"upstreamHost"`   // optional. Overrides the Host header (and TLS SNI) sent upstream. Default: keep the incoming Host.
    InsecureSkipVerify bool `json:"insecureSkipVerify"` // only for upstreamScheme https. Default false.
    DialTimeout    string `json:"dialTimeout"`    // Go duration, default "15s".
}
```

`CreateConfig` returns the defaults. `New` validates: `Service` and
`IdentityFile` non-empty, `KeyAlg` and `UpstreamScheme` in their allowed sets,
`DialTimeout` parses. Validation errors are returned from `New` with the
middleware `name` in the message.

### 4.2 Identity bootstrap

Function `loadOrEnroll(cfg *Config) (*ziti.Config, error)`:

1. If `IdentityFile` exists and is non-empty: `ziti.NewConfigFromFile`. Ignore `JWTFile` entirely.
2. Otherwise `JWTFile` must be set and readable; if not, return an error that
   names both paths.
3. Read the JWT, `strings.TrimSpace`, `enroll.ParseToken`, then `enroll.Enroll`
   with `Token`, `JwtToken`, `KeyAlg`.
4. Marshal the returned config to JSON. Write it to `IdentityFile + ".tmp"`
   with mode `0600`, then `os.Rename` to `IdentityFile`. Create the parent
   directory with `0700` if missing.
5. If the write fails after a successful enrollment, return an error saying
   the JWT is now consumed and a new one must be issued. Do not delete the JWT file.
6. Never log the JWT or the identity contents.

### 4.3 Shared Ziti context

Traefik calls `New` once per router that uses the middleware and again on every
dynamic-config reload. Enrollment must run once and contexts must be shared:

- Package-level `map[string]*entry` keyed by absolute `IdentityFile`, guarded
  by a `sync.Mutex`. `entry` holds the `ziti.Context` and the init error.
- First `New` for a key does `loadOrEnroll` + `ziti.NewContext` + `Authenticate()`
  under the lock; later calls reuse the entry.
- A failed init is not cached; the next `New` retries.
- Contexts live for the process lifetime; do not close them when the `ctx`
  passed to `New` is cancelled (reloads would tear down live connections).

After authenticating, check the service with `GetServices()` and log a warning
(not an error) if `Service` is not visible or lacks dial permission. Service
policies can be granted later.

### 4.4 Request handling

- Build one `http.Transport` per middleware instance with
  `DialContext: func(ctx, _, _) { return zitiCtx.DialContext(ctx, cfg.Service) }`.
  The network/address arguments are ignored; the Ziti service decides the destination.
  Apply `DialTimeout` with `context.WithTimeout`.
  Set `TLSClientConfig` only for `upstreamScheme: https` (ServerName =
  `UpstreamHost` or incoming host, `InsecureSkipVerify` from config).
  Set `MaxIdleConnsPerHost` to 32, `IdleConnTimeout` to 90s.
- Use `httputil.ReverseProxy` with a `Rewrite` func (not `Director`):
  set `URL.Scheme`, set `URL.Host` to `UpstreamHost` or the incoming `Host`,
  keep the incoming `Host` header unless `UpstreamHost` is set, call
  `SetXForwarded()`.
- `FlushInterval: -1` so streaming and SSE work. WebSocket upgrades work
  through `ReverseProxy` unchanged; cover them with a test.
- `ErrorHandler`: log service name and error, respond `502 Bad Gateway` for
  dial/transport failures and `504 Gateway Timeout` when the error is a
  context deadline. Body is a short plain-text message without internal details.
- `ServeHTTP` never calls `next`.

### 4.5 Logging

Standard library `log` to stdout/stderr with prefix `[openziti-proxy]`.
Log once per event: enrollment performed, identity loaded, context
authenticated, service not visible, each proxy error.

## 5. Files to deliver

```
.traefik.yml
go.mod, go.sum, vendor/
openziti.go            // Config, CreateConfig, New, handler
identity.go            // loadOrEnroll, shared context registry
openziti_test.go
identity_test.go
README.md
examples/traefik.yml           // static config, local-plugin mode
examples/dynamic.yml           // router + middleware
examples/docker-compose.yml    // Traefik with the plugin and a /ziti volume
.github/workflows/ci.yml       // go vet, go test, yaegi load check
LICENSE                        // Apache-2.0
```

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
          jwtFile: /ziti/enroll.jwt
          identityFile: /ziti/identity.json
```

README sections: what it does (with the diagram from section 1), prerequisites
on the Ziti side (identity created with a JWT, dial service policy for that
identity, service hosted with a host.v1 config, edge router policies),
installation, config reference table, the enrollment lifecycle (JWT is
one-time; identity file must be on a persistent volume; delete the identity
file and supply a new JWT to re-enroll), troubleshooting.

## 6. Tests and acceptance

Unit tests, no Ziti network needed. To make that possible, put the dialer
behind a small interface (`DialContext(ctx, service) (net.Conn, error)`) and
inject a fake that connects to an `httptest.Server`.

- Config validation: each invalid field produces an error.
- `loadOrEnroll`: existing identity file is used and the JWT is not read;
  missing identity and missing JWT gives a clear error; atomic write leaves no
  `.tmp` file and the result has mode `0600` (inject the enroll function).
- Proxying: GET and POST bodies round-trip; Host header behaviour with and
  without `UpstreamHost`; `X-Forwarded-*` set; dial failure gives 502;
  dial timeout gives 504; WebSocket upgrade round-trips.
- Shared registry: two `New` calls with the same identity file enroll once
  (run with `-race`).

Done means all of:

1. `go vet ./...` and `go test -race ./...` pass.
2. Traefik (same version as the spike) loads the plugin in local mode with no
   Yaegi errors, verified by actually running it.
3. If an OpenZiti network is reachable from the environment (e.g. the
   `openziti/ziti-controller` quickstart container), run one end-to-end
   request through Traefik to a host.v1 destination and include the result in
   the report. If not reachable, say so explicitly; do not claim end-to-end
   verification.
4. Everything is committed and pushed to branch `claude/clever-ramanujan-1vbieo`.
   Do not open a pull request.

## 7. Fallbacks if the Phase 1 gate fails (for the report only)

- **A. Plugin + sidecar.** A dependency-free Yaegi plugin forwards each request
  to a small Go binary in this repo (`cmd/ziti-sidecar`, HTTP over a Unix
  socket or localhost). The sidecar does everything in sections 4.2 to 4.4.
  Still installable from the plugin catalog, one extra process.
- **B. Sidecar only.** Skip the plugin; Traefik uses the sidecar as a normal
  `loadBalancer` server URL. Simplest, but no longer a "Traefik plugin".
- **C. Custom Traefik build.** Compile the middleware into a Traefik fork.
  No interpreter limits, not distributable via the catalog.

## 8. Rules

- Do not put secrets, real JWTs or identity files in the repo; add
  `*.jwt` and `identity*.json` to `.gitignore`.
- Do not add config fields, features or abstractions beyond this document.
- When a fact here conflicts with what the vendored SDK or the Traefik docs
  show, trust the source, and mention the difference in the final report.
- Report honestly: failed steps are listed as failed with their output.
