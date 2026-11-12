#!/usr/bin/env bash
# Container smoke test with podman.
#
# 1. Builds the sidecar image from sidecar/Containerfile and checks that the
#    binary starts and rejects a missing --identity-file.
# 2. Runs Traefik in a pod with the plugin in local-plugin mode, using the
#    unmodified examples/traefik.yml and examples/dynamic.yml, and a header-echo
#    container standing in for the sidecar on port 8900. Both Traefik
#    entrypoints are published: :80 on SMOKE_HTTP_PORT and :443 on SMOKE_HTTPS_PORT.
# 3. Sends a plain HTTP request to http://service.ziti and an HTTPS request to
#    https://service.ziti through Traefik, and checks what the stand-in sidecar
#    received for each. The HTTPS certificate is not verified (curl -k).
#
# It does not need an OpenZiti network and does not test enrollment or dialing.
#
# PODMAN_BUILD_ARGS adds arguments to "podman build", for example to trust a
# corporate proxy CA: PODMAN_BUILD_ARGS="-v /path/ca.crt:/etc/ssl/certs/ca-certificates.crt:ro"
#
# SMOKE_HTTP_PORT and SMOKE_HTTPS_PORT choose the host ports on 127.0.0.1
# (default 18080 and 18443).
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
module=github.com/raskhadafi/traefik-openziti-proxy
image=localhost/traefik-openziti-sidecar:smoke
traefik_image="${TRAEFIK_IMAGE:-docker.io/library/traefik:v3}"
echo_image="${ECHO_IMAGE:-docker.io/traefik/whoami:v1.11}"
http_port="${SMOKE_HTTP_PORT:-18080}"
https_port="${SMOKE_HTTPS_PORT:-18443}"
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
podman pod create --name "$pod" \
  -p "127.0.0.1:$http_port:80" -p "127.0.0.1:$https_port:443" \
  --add-host ziti-sidecar:127.0.0.1 >/dev/null
podman run -d --pod "$pod" --name "$pod-sidecar" "$echo_image" --port 8900 >/dev/null
podman run -d --pod "$pod" --name "$pod-traefik" \
  -v "$repo/examples/traefik.yml:/etc/traefik/traefik.yml:ro" \
  -v "$repo/examples/dynamic.yml:/etc/traefik/dynamic.yml:ro" \
  -v "$repo:/plugins-local/src/$module:ro" \
  "$traefik_image" >/dev/null

expect() { grep -qi -- "$1" <<<"$body" || fail "response is missing '$1': $body"; }

# check_request URL HOST_PORT FORWARDED_PROTO [curl options...]
# Retries until Traefik answers (60 tries, 1s apart), then checks the headers
# the stand-in sidecar received. The response body is left in $body.
check_request() {
  local url="$1" port="$2" proto="$3"
  shift 3
  local errfile last_err=""
  errfile="$(mktemp)"

  echo "== request $url through Traefik"
  body=""
  for _ in $(seq 1 60); do
    if body="$(curl -sS --fail --max-time 5 --noproxy '*' "$@" \
        --resolve "service.ziti:$port:127.0.0.1" \
        -H 'X-Ziti-Service: evil' \
        "$url" 2>"$errfile")"; then
      break
    fi
    body=""
    last_err="$(cat "$errfile")"
    sleep 1
  done
  rm -f "$errfile"
  [ -n "$body" ] || fail "no 200 response from $url within 60s; last curl error: ${last_err:-none}"
  # The echoed request uses CRLF line endings; drop the CRs so line-anchored checks match.
  body="${body//$'\r'/}"

  expect '^X-Ziti-Service: my-service'
  expect '^X-Ziti-Upstream-Scheme: http'
  expect '^Host: service.ziti'
  expect "^X-Forwarded-Proto: ${proto}\$"
  expect '^GET /smoke?q=1 '
  if grep -qi 'evil' <<<"$body"; then
    fail "client-supplied X-Ziti-Service reached the sidecar: $body"
  fi
}

check_request "http://service.ziti:$http_port/smoke?q=1" "$http_port" http
check_request "https://service.ziti:$https_port/smoke?q=1" "$https_port" https -k

# Traefik logs only errors by default; the plugin headers above already prove
# that the plugin was loaded.
echo "== traefik log has no errors"
log="$(podman logs "$pod-traefik" 2>&1)"
if grep -Eq ' (ERR|FTL|PNC) ' <<<"$log"; then
  fail "Traefik logged errors"
fi

echo "PASS"
