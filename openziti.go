// Package traefik_openziti_proxy is a Traefik middleware that forwards each
// request to a companion sidecar and tells it which Ziti service to dial.
package traefik_openziti_proxy

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const (
	defaultSidecarAddress = "http://127.0.0.1:8900"

	headerService  = "X-Ziti-Service"
	headerScheme   = "X-Ziti-Upstream-Scheme"
	headerHost     = "X-Ziti-Upstream-Host"
	controlPrefix  = "X-Ziti-"
	unixTargetHost = "sidecar"
)

// Config is the plugin configuration.
type Config struct {
	Service        string `json:"service"`
	SidecarAddress string `json:"sidecarAddress"`
	UpstreamScheme string `json:"upstreamScheme"`
	UpstreamHost   string `json:"upstreamHost"`
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		SidecarAddress: defaultSidecarAddress,
		UpstreamScheme: "http",
	}
}

type handler struct {
	proxy *httputil.ReverseProxy
}

// New creates the middleware. It never contacts the sidecar.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if config == nil {
		return nil, fmt.Errorf("openziti middleware %q: missing configuration", name)
	}
	if strings.TrimSpace(config.Service) == "" {
		return nil, fmt.Errorf("openziti middleware %q: service must not be empty", name)
	}
	scheme := config.UpstreamScheme
	if scheme == "" {
		scheme = "http"
	}
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("openziti middleware %q: upstreamScheme must be http or https, got %q", name, config.UpstreamScheme)
	}
	addr := config.SidecarAddress
	if addr == "" {
		addr = defaultSidecarAddress
	}

	transport := &http.Transport{
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
	var target *url.URL
	if strings.HasPrefix(addr, "unix://") {
		path := strings.TrimPrefix(addr, "unix://")
		if !strings.HasPrefix(path, "/") || len(path) < 2 {
			return nil, fmt.Errorf("openziti middleware %q: sidecarAddress %q: unix address must be unix:///absolute/path", name, addr)
		}
		transport.DialContext = func(c context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(c, "unix", path)
		}
		target = &url.URL{Scheme: "http", Host: unixTargetHost}
	} else {
		u, err := url.Parse(addr)
		if err != nil {
			return nil, fmt.Errorf("openziti middleware %q: sidecarAddress %q: %v", name, addr, err)
		}
		if u.Scheme != "http" || u.Hostname() == "" || u.Port() == "" || (u.Path != "" && u.Path != "/") {
			return nil, fmt.Errorf("openziti middleware %q: sidecarAddress %q must be http://host:port or unix:///absolute/path", name, addr)
		}
		target = &url.URL{Scheme: "http", Host: u.Host}
	}

	service := config.Service
	upstreamHost := config.UpstreamHost

	director := func(req *http.Request) {
		origHost := req.Host
		proto := "http"
		if req.TLS != nil {
			proto = "https"
		}
		fwdProto := proto
		if v := req.Header.Get("X-Forwarded-Proto"); v != "" {
			fwdProto = v
		}
		fwdHost := origHost
		if v := req.Header.Get("X-Forwarded-Host"); v != "" {
			fwdHost = v
		}

		for key := range req.Header {
			if len(key) >= len(controlPrefix) && strings.EqualFold(key[:len(controlPrefix)], controlPrefix) {
				delete(req.Header, key)
			}
		}
		req.Header.Set(headerService, service)
		req.Header.Set(headerScheme, scheme)
		if upstreamHost != "" {
			req.Header.Set(headerHost, upstreamHost)
		}

		// X-Forwarded-For is appended by httputil.ReverseProxy itself.
		req.Header.Set("X-Forwarded-Proto", fwdProto)
		req.Header.Set("X-Forwarded-Host", fwdHost)

		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = origHost
	}

	rp := &httputil.ReverseProxy{
		Director:      director,
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			log.Printf("openziti middleware %q: sidecar request failed: %v", name, err)
			rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
			rw.WriteHeader(http.StatusBadGateway)
			_, _ = rw.Write([]byte("Bad Gateway\n"))
		},
	}
	return &handler{proxy: rp}, nil
}

func (h *handler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	h.proxy.ServeHTTP(rw, req)
}
