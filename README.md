# Traefik OpenZiti proxy

Turns Traefik into a public ingress for an [OpenZiti](https://openziti.io)
overlay network. Browsers need no Ziti software.

## What it does

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

Two parts live in this repository:

- **Plugin** (repository root): a small, dependency-free Traefik middleware.
  It forwards each request to the sidecar and tells it which Ziti service to use.
  It is terminal: it answers the request itself and never calls the next handler,
  so the router still needs a `service`; the examples use `noop@internal`.
- **Sidecar** (`sidecar/`): a standalone Go binary that holds the Ziti identity,
  completes enrollment, connects to the Ziti network and dials services.

Hostname and port matching (`test.ch`, `:443`) is done by Traefik itself with a
router rule and an entrypoint. Nothing in this project's config names a host or port.

## Why a sidecar

The OpenZiti Go SDK requires Go 1.26 and pulls in about 315 third-party
packages, some using unsafe system calls. Traefik's plugin interpreter (Yaegi)
cannot be relied on to load that. The plugin therefore stays free of
dependencies, and the JWT and identity file are configured on the sidecar, not in
the Traefik plugin config.

## Prerequisites on the Ziti side

This project dials a service; it does not create or host one.

1. An identity for the sidecar, created with an enrollment JWT
   (`ziti edge create identity sidecar-id -a sidecars -o sidecar.jwt`).
2. A **Dial** service policy that grants that identity access to the service.
3. The service itself, hosted by another identity or tunneler through a
   `host.v1` config that points at the real destination.
4. Edge router policy and service edge router policy so that the identity and the
   service share at least one edge router.

## Quick start

```sh
cd examples
cp /path/to/sidecar.jwt ./identity.jwt      # one-time enrollment JWT (git-ignored)
# edit dynamic.yml: rule, service name; edit compose.yml: --allowed-services
podman compose up --build
```

The container runtime is podman. `podman compose` needs a compose provider such
as `podman-compose`. Rootless podman cannot publish ports below 1024 by
default, so the example publishes Traefik on 8080 and 8443; put a port
forward in front or lower `net.ipv4.ip_unprivileged_port_start` to use 80 and
443. On SELinux hosts append `,z` to the bind mounts in `compose.yml`.

The compose file mounts the repository into Traefik at
`/plugins-local/src/github.com/raskhadafi/traefik-openziti-proxy` (local plugin
mode), runs the sidecar with `--listen 0.0.0.0:8900`, keeps the identity file in
the named volume `ziti-identity` and does not publish the sidecar port. The
example router in `examples/dynamic.yml` uses `tls: {}` on the `websecure`
entrypoint; supply a certificate or a certificate resolver for real use.

## Plugin configuration

```yaml
http:
  middlewares:
    to-ziti:
      plugin:
        openziti:
          service: my-service
          sidecarAddress: http://ziti-sidecar:8900
```

| Field | Default | Meaning |
|---|---|---|
| `service` | required | Ziti service name to dial. |
| `sidecarAddress` | `http://127.0.0.1:8900` | `http://host:port` or `unix:///absolute/path.sock`. |
| `upstreamScheme` | `http` | `http` or `https`: protocol spoken to the destination inside the Ziti connection. |
| `upstreamHost` | empty | Overrides the Host header (and TLS SNI) sent to the destination. Default: the incoming Host. |

The plugin removes every incoming `X-Ziti-*` header before adding its own, so a
client can never choose the service. It does not contact the sidecar at startup;
the sidecar may start later than Traefik. If the sidecar is unreachable the
client gets `502`.

### Plugin to sidecar protocol

Plain HTTP/1.1 with the original request unchanged, plus `X-Ziti-Service`
(required), `X-Ziti-Upstream-Scheme` and `X-Ziti-Upstream-Host` (optional). The
sidecar strips all `X-Ziti-*` headers before forwarding. Without
`X-Ziti-Service`, `GET /healthz` answers `200 ok` when the Ziti controller is
reachable with the loaded identity and `503` otherwise; any other request gets `400`.

## Sidecar flags

Each flag has an environment variable fallback; the flag wins.

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--listen` | `ZITI_SIDECAR_LISTEN` | `127.0.0.1:8900` | `host:port` or `unix:///path/to.sock` (stale socket removed, mode `0660`). |
| `--identity-file` | `ZITI_IDENTITY_FILE` | required | Where the enrolled identity JSON is stored. |
| `--jwt-file` | `ZITI_JWT_FILE` | empty | One-time enrollment JWT. Needed only when the identity file does not exist yet. |
| `--key-alg` | `ZITI_KEY_ALG` | `EC` | `EC` or `RSA`. |
| `--allowed-services` | `ZITI_ALLOWED_SERVICES` | empty = all | Comma-separated allowlist. Others get `403`. |
| `--dial-timeout` | `ZITI_DIAL_TIMEOUT` | `15s` | Go duration. A timed-out dial gives `504`. |
| `--insecure-skip-verify` | `ZITI_INSECURE_SKIP_VERIFY` | `false` | Skip certificate verification for `https` upstreams only. |

Build and test: `cd sidecar && go build . && go test -race ./...`.

## Enrollment lifecycle

- The JWT is **one-time**. On first start the sidecar reads it, enrolls, and
  writes the identity to `--identity-file` (mode `0600`, via a temporary file and
  rename). Enrollment needs network access to the controller.
- On later starts the identity file is used and the JWT file is ignored.
- Keep the identity file on a **persistent volume**. If it is lost after the JWT
  was consumed, enrollment cannot be repeated with the same JWT.
- If enrollment succeeds but the identity cannot be written, the sidecar says so:
  the JWT is consumed and a new one must be issued. The JWT file is never deleted.
- To re-enroll, delete the identity file and supply a new JWT.

## Security notes

- Keep the sidecar listener private: it trusts `X-Ziti-Service` from whoever can
  reach it. Do not publish its port; prefer a private network or a unix socket.
- Set `--allowed-services` to the services your routers use.
- The identity file is a credential. Protect it like a private key.
- The JWT and identity contents are never logged. Do not commit `*.jwt` or
  `identity*.json` (both are in `.gitignore`).

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `502` with body `Bad Gateway` from Traefik | Plugin cannot reach the sidecar: check `sidecarAddress`, network and that the sidecar is running. |
| `502` from the sidecar | Dial failed: the identity has no Dial policy for the service, no common edge router, or nothing hosts the service. See the sidecar log (`proxy error`). |
| `504` | Dial exceeded `--dial-timeout`. |
| `403` | Service is not in `--allowed-services`. |
| `400` | Request reached the sidecar without `X-Ziti-Service`; it did not come through the plugin. |
| Sidecar exits at start | Missing identity and JWT, bad JWT, unreachable controller, or failed authentication. The message names the paths. |
| Warning `service not dialable` | A name in `--allowed-services` is not visible to the identity yet; grant a Dial policy, no restart needed. |
| Plugin does not load in Traefik | The repository must be at `/plugins-local/src/github.com/raskhadafi/traefik-openziti-proxy` and `experimental.localPlugins` must be set. |

## Testing

```sh
go vet ./... && go test -race ./...                      # plugin
(cd sidecar && go vet ./... && go test -race ./...)      # sidecar
./test/podman-smoke.sh                                   # containers, needs podman
```

`test/podman-smoke.sh` builds the sidecar image from `sidecar/Containerfile`,
then runs Traefik in a podman pod with the plugin and the example configs, with
a header-echo container in place of the sidecar. It requests `https://test.ch`
and checks the headers the plugin sent. It does not need an OpenZiti network
and does not cover enrollment or dialing.

## License

Apache-2.0, see `LICENSE`.
