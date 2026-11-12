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
)

type dialer interface {
	DialContext(ctx context.Context, service string) (net.Conn, error)
}

type transportKey struct {
	service, scheme, host string
}

type proxyHandler struct {
	dialer       dialer
	allowed      map[string]bool // empty = all
	dialTimeout  time.Duration
	insecureSkip bool
	ready        func() bool

	mu         sync.Mutex
	transports map[transportKey]*http.Transport
	rp         *httputil.ReverseProxy
}

func newProxyHandler(d dialer, allowed []string, dialTimeout time.Duration, insecureSkip bool, ready func() bool) *proxyHandler {
	h := &proxyHandler{
		dialer:       d,
		allowed:      map[string]bool{},
		dialTimeout:  dialTimeout,
		insecureSkip: insecureSkip,
		ready:        ready,
		transports:   map[transportKey]*http.Transport{},
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
	}
	return h
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type ctxKey struct{}

type route struct {
	key transportKey
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
	ctx := context.WithValue(r.Context(), ctxKey{}, route{key: transportKey{service, scheme, host}})
	h.rp.ServeHTTP(w, r.WithContext(ctx))
}

func (h *proxyHandler) rewrite(pr *httputil.ProxyRequest) {
	rt := pr.In.Context().Value(ctxKey{}).(route)
	pr.Out.URL = &url.URL{
		Scheme:   rt.key.scheme,
		Host:     rt.key.host,
		Path:     pr.In.URL.Path,
		RawPath:  pr.In.URL.RawPath,
		RawQuery: pr.In.URL.RawQuery,
	}
	pr.Out.Host = rt.key.host
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
	return h.transport(rt.key).RoundTrip(r)
}

func (h *proxyHandler) transport(k transportKey) *http.Transport {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.transports[k]; ok {
		return t
	}
	t := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, h.dialTimeout)
			defer cancel()
			return h.dialer.DialContext(ctx, k.service)
		},
		MaxIdleConnsPerHost: maxIdlePerHost,
		IdleConnTimeout:     90 * time.Second,
	}
	if k.scheme == "https" {
		t.TLSClientConfig = &tls.Config{
			ServerName:         hostWithoutPort(k.host),
			InsecureSkipVerify: h.insecureSkip,
		}
	}
	h.transports[k] = t
	return t
}

func hostWithoutPort(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return strings.Trim(h, "[]")
}

func (h *proxyHandler) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	service := r.Header.Get(headerService)
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
