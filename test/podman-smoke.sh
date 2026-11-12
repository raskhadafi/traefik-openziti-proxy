#!/usr/bin/env bash
# Container smoke test with podman.
#
# 1. Builds the sidecar image from sidecar/Containerfile and checks that the
#    binary starts and rejects a missing --identity-file.
# 2. Runs Traefik in a pod with the plugin in local-plugin mode, using the
#    unmodified examples/traefik.yml and examples/dynamic.yml, and a header-echo
#    container standing in for the sidecar on port 8900.
# 3. Sends an HTTPS request for test.ch through Traefik and checks what the
#    stand-in sidecar received.
#
# It does not need an OpenZiti network and does not test enrollment or dialing.
#
# PODMAN_BUILD_ARGS adds arguments to "podman build", for example to trust a
# corporate proxy CA: PODMAN_BUILD_ARGS="-v /path/ca.crt:/etc/ssl/certs/ca-certificates.crt:ro"
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
module=github.com/raskhadafi/traefik-openziti-proxy
image=localhost/traefik-openziti-sidecar:smoke
traefik_image="${TRAEFIK_IMAGE:-docker.io/library/traefik:v3}"
echo_image="${ECHO_IMAGE:-docker.io/traefik/whoami:v1.11}"
port="${SMOKE_PORT:-18443}"
pod=openziti-smoke

fail() {
  echo "FAIL: $*" >&2
  echo "--- traefik log ---" >&2
  podman logs "$pod-traefik" >&2 2>&1 || true
  exit 1
}
cleanup() {  podman pod rm -f "$pod" >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

echo "== build sidecar image"
# shellcheck disable=SC2086
podman build -q ${PODMAN_BUILD_ARGS:-} -t "$image" -f "$repo/sidecar/Containerfile" "$repo/sidecar"

echo "== sidecar image rejects missing --identity-file"
if out="$(podman run --rm "$image" 2>&1)"; then
  fail "sidecar exited 0 without --identity-file"
fi
grep -q "identity-file" <<<"$out" || fail "unexpected sidecar output: $out"

echo "== start pod"
podman pod create --name "$pod" -p "127.0.0.1:$port:443" \
  --add-host ziti-sidecar:127.0.0.1 >/dev/null
podman run -d --pod "$pod" --name "$pod-sidecar" "$echo_image" --port 8900 >/dev/null
podman run -d --pod "$pod" --name "$pod-traefik" \
  -v "$repo/examples/traefik.yml:/etc/traefik/traefik.yml:ro" \
  -v "$repo/examples/dynamic.yml:/etc/traefik/dynamic.yml:ro" \
  -v "$repo:/plugins-local/src/$module:ro" \
  "$traefik_image" >/dev/null

echo "== request https://test.ch through Traefik"
body=""
for _ in $(seq 1 60); do
  if body="$(curl -sS -k --fail --max-time 5 --noproxy '*' \
      --resolve "test.ch:$port:127.0.0.1" \
      -H 'X-Ziti-Service: evil' \
      "https://test.ch:$port/smoke?q=1" 2>/dev/null)"; then
    break
  fi
  body=""
  sleep 1
done
[ -n "$body" ] || fail "no 200 response from Traefik within 60s"

expect() { grep -qi -- "$1" <<<"$body" || fail "response is missing '$1': $body"; }
expect '^X-Ziti-Service: my-service'
expect '^X-Ziti-Upstream-Scheme: http'
expect '^Host: test.ch'
expect '^X-Forwarded-Proto: https'
expect '^GET /smoke?q=1 '
if grep -qi 'evil' <<<"$body"; then
  fail "client-supplied X-Ziti-Service reached the sidecar: $body"
fi

# Traefik logs only errors by default; the plugin headers above already prove
# that the plugin was loaded.
echo "== traefik log has no errors"
log="$(podman logs "$pod-traefik" 2>&1)"
if grep -Eq ' (ERR|FTL|PNC) ' <<<"$log"; then
  fail "Traefik logged errors"
fi

echo "PASS"
