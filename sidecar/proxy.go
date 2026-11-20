package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	headerService  = "X-Ziti-Service"
	headerScheme   = "X-Ziti-Upstream-Scheme"
	headerHost     = "X-Ziti-Upstream-Host"
	controlPrefix  = "X-Ziti-"
	maxIdlePerHost = 32

	// maxTransports bounds the number of per-service transports kept. It is
	// only reachable when no service allowlist is configured.
	maxTransports = 256

	// plainHTTPTarget is the URL host used for plain-HTTP upstream requests.
	// The dialer ignores the address, so every plain-HTTP request for a
	// service shares one idle pool regardless of its Host header. The Host
	// header sent upstream is still the real upstream host.
	plainHTTPTarget = "ziti-upstream"
)

type dialer interface {
	DialContext(ctx context.Context, service string) (net.Conn, error)
}

type proxyHandler struct {
	dialer       dialer
	allowed      map[string]bool // empty = all
	dialTimeout  time.Duration
	insecureSkip bool
	ready        func() bool

	mu         sync.Mutex
	transports map[string]*http.Transport // keyed by Ziti service name
	rp         *httputil.ReverseProxy
}

func newProxyHandler(d dialer, allowed []string, dialTimeout time.Duration, insecureSkip bool, ready func() bool) *proxyHandler {
	h := &proxyHandler{
		dialer:       d,
		allowed:      map[string]bool{},
		dialTimeout:  dialTimeout,
		insecureSkip: insecureSkip,
		ready:        ready,
		transports:   map[string]*http.Transport{},
	}
	for _, s := range allowed {
		if s = strings.TrimSpace(s); s != "" {
			h.allowed[s] = true
		}
	}
	h.rp = &httputil.ReverseProxy{
		Rewrite:       h.rewrite,
		FlushInterval: -1,
		Transport:     http.RoundTripper(roundTripperFunc(h.roundTrip)),
		ErrorHandler:  h.errorHandler,
		BufferPool:    bufferPool{},
	}
	return h
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// bufferPool reuses the 32 KiB copy buffers of the reverse proxy.
type bufferPool struct{}

var copyBuffers = sync.Pool{New: func() any {
	b := make([]byte, 32<<10)
	return &b
}}

func (bufferPool) Get() []byte {
	return *copyBuffers.Get().(*[]byte)
}

func (bufferPool) Put(b []byte) {
	copyBuffers.Put(&b)
}

type ctxKey struct{}

type route struct {
	service, scheme, host string
}

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	service := r.Header.Get(headerService)
	if service == "" {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			if h.ready != nil && h.ready() {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				_, _ = w.Write([]byte("ok"))
				return
			}
			http.Error(w, "not authenticated", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "missing "+headerService+" header", http.StatusBadRequest)
		return
	}
	if len(h.allowed) > 0 && !h.allowed[service] {
		http.Error(w, "service not allowed", http.StatusForbidden)
		return
	}
	scheme := r.Header.Get(headerScheme)
	if scheme == "" {
		scheme = "http"
	}
	if scheme != "http" && scheme != "https" {
		http.Error(w, "invalid "+headerScheme+" header", http.StatusBadRequest)
		return
	}
	host := r.Header.Get(headerHost)
	if host == "" {
		host = r.Host
	}
	ctx := context.WithValue(r.Context(), ctxKey{}, route{service: service, scheme: scheme, host: host})
	h.rp.ServeHTTP(w, r.WithContext(ctx))
}

func (h *proxyHandler) rewrite(pr *httputil.ProxyRequest) {
	rt := pr.In.Context().Value(ctxKey{}).(route)
	target := rt.host
	if rt.scheme == "http" {
		target = plainHTTPTarget
	}
	pr.Out.URL = &url.URL{
		Scheme:   rt.scheme,
		Host:     target,
		Path:     pr.In.URL.Path,
		RawPath:  pr.In.URL.RawPath,
		RawQuery: pr.In.URL.RawQuery,
	}
	pr.Out.Host = rt.host
	// The plugin already set X-Forwarded-*; keep them as they are.
	for k := range pr.Out.Header {
		if len(k) >= len(controlPrefix) && strings.EqualFold(k[:len(controlPrefix)], controlPrefix) {
			delete(pr.Out.Header, k)
		}
	}
	for _, k := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host"} {
		if v, ok := pr.In.Header[k]; ok {
			pr.Out.Header[k] = v
		}
	}
}

func (h *proxyHandler) roundTrip(r *http.Request) (*http.Response, error) {
	rt := r.Context().Value(ctxKey{}).(route)
	return h.transport(rt.service).RoundTrip(r)
}

// transport returns the shared upstream transport for a Ziti service. For
// https the TLS server name is taken from the request URL host, so each
// upstream host gets its own connection pool and correct SNI.
func (h *proxyHandler) transport(service string) *http.Transport {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.transports[service]; ok {
		return t
	}
	if len(h.transports) >= maxTransports {
		// Idle connections are Ziti circuits; release them before forgetting
		// the transports. In-flight requests finish on the old transports.
		for _, t := range h.transports {
			t.CloseIdleConnections()
		}
		h.transports = map[string]*http.Transport{}
	}
	t := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, h.dialTimeout)
			defer cancel()
			return h.dialer.DialContext(ctx, service)
		},
		MaxIdleConnsPerHost: maxIdlePerHost,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: h.insecureSkip,
		},
	}
	h.transports[service] = t
	return t
}

func (h *proxyHandler) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	service := r.Header.Get(headerService)
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		// The client went away; there is nobody to answer.
		slog.Debug("client cancelled request", "service", service, "error", err)
		return
	}
	slog.Error("proxy error", "service", service, "error", err)
	status, msg := http.StatusBadGateway, "Bad Gateway"
	if errors.Is(err, context.DeadlineExceeded) {
		status, msg = http.StatusGatewayTimeout, "Gateway Timeout"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg + "\n"))
}

// closeIdle releases idle upstream connections.
func (h *proxyHandler) closeIdle() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range h.transports {
		t.CloseIdleConnections()
	}
}
