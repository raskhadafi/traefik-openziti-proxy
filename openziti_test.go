package traefik_openziti_proxy

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type seen struct {
	host, method, path, query, body string
	header                          http.Header
}

func newSidecar(t *testing.T, status int) (*httptest.Server, *seen) {
	t.Helper()
	s := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.host, s.method, s.path, s.query, s.body = r.Host, r.Method, r.URL.Path, r.URL.RawQuery, string(b)
		s.header = r.Header.Clone()
		w.WriteHeader(status)
		_, _ = w.Write([]byte("from-sidecar"))
	}))
	t.Cleanup(srv.Close)
	return srv, s
}

func mustNew(t *testing.T, cfg *Config) http.Handler {
	t.Helper()
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("next must never be called")
	}), cfg, "test-mw")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestConfigValidation(t *testing.T) {
	cases := map[string]func(*Config){
		"empty service":      func(c *Config) { c.Service = "" },
		"blank service":      func(c *Config) { c.Service = "  " },
		"bad scheme":         func(c *Config) { c.UpstreamScheme = "ftp" },
		"bad sidecar scheme": func(c *Config) { c.SidecarAddress = "https://127.0.0.1:8900" },
		"no port":            func(c *Config) { c.SidecarAddress = "http://127.0.0.1" },
		"garbage":            func(c *Config) { c.SidecarAddress = "://" },
		"relative unix":      func(c *Config) { c.SidecarAddress = "unix://relative.sock" },
		"empty unix":         func(c *Config) { c.SidecarAddress = "unix://" },
		"path in url":        func(c *Config) { c.SidecarAddress = "http://127.0.0.1:8900/x" },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := CreateConfig()
			cfg.Service = "svc"
			mod(cfg)
			_, err := New(context.Background(), nil, cfg, "my-mw")
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), "my-mw") {
				t.Fatalf("error should name the middleware: %v", err)
			}
		})
	}
	cfg := CreateConfig()
	cfg.Service = "svc"
	if _, err := New(context.Background(), nil, cfg, "ok"); err != nil {
		t.Fatalf("default config with service should be valid (and must not contact sidecar): %v", err)
	}
}

func TestHeadersAndPassthrough(t *testing.T) {
	srv, s := newSidecar(t, 200)
	cfg := CreateConfig()
	cfg.Service = "my-service"
	cfg.SidecarAddress = srv.URL
	cfg.UpstreamScheme = "https"
	cfg.UpstreamHost = "internal.example"
	h := mustNew(t, cfg)

	req := httptest.NewRequest("POST", "http://test.ch/a/b?x=1&y=2", strings.NewReader("payload"))
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("X-Ziti-Service", "evil")
	req.Header.Set("x-ziti-other", "evil")
	req.Header.Set("X-Custom", "keep")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 || rec.Body.String() != "from-sidecar" {
		t.Fatalf("unexpected response %d %q", rec.Code, rec.Body.String())
	}
	if s.host != "test.ch" {
		t.Errorf("host = %q", s.host)
	}
	if s.method != "POST" || s.path != "/a/b" || s.query != "x=1&y=2" || s.body != "payload" {
		t.Errorf("request altered: %+v", s)
	}
	if got := s.header.Values("X-Ziti-Service"); len(got) != 1 || got[0] != "my-service" {
		t.Errorf("X-Ziti-Service = %v", got)
	}
	if got := s.header.Get("X-Ziti-Upstream-Scheme"); got != "https" {
		t.Errorf("scheme header = %q", got)
	}
	if got := s.header.Get("X-Ziti-Upstream-Host"); got != "internal.example" {
		t.Errorf("host header = %q", got)
	}
	if s.header.Get("X-Ziti-Other") != "" {
		t.Error("client X-Ziti-Other leaked")
	}
	if s.header.Get("X-Custom") != "keep" {
		t.Error("custom header lost")
	}
	if got := s.header.Values("X-Forwarded-For"); len(got) != 1 || got[0] != "203.0.113.9" {
		t.Errorf("X-Forwarded-For = %v", got)
	}
	if got := s.header.Get("X-Forwarded-Proto"); got != "http" {
		t.Errorf("X-Forwarded-Proto = %q", got)
	}
	if got := s.header.Get("X-Forwarded-Host"); got != "test.ch" {
		t.Errorf("X-Forwarded-Host = %q", got)
	}
}

func TestClientUpstreamHostRemovedWhenNotConfigured(t *testing.T) {
	srv, s := newSidecar(t, 200)
	cfg := CreateConfig()
	cfg.Service = "svc"
	cfg.SidecarAddress = srv.URL
	h := mustNew(t, cfg)
	req := httptest.NewRequest("GET", "http://test.ch/", nil)
	req.Header.Set("X-Ziti-Upstream-Host", "attacker")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if s.header.Get("X-Ziti-Upstream-Host") != "" {
		t.Error("client-supplied upstream host reached the sidecar")
	}
	if s.header.Get("X-Ziti-Upstream-Scheme") != "http" {
		t.Errorf("default scheme = %q", s.header.Get("X-Ziti-Upstream-Scheme"))
	}
}

func TestForwardedProtoTLS(t *testing.T) {
	srv, s := newSidecar(t, 200)
	cfg := CreateConfig()
	cfg.Service = "svc"
	cfg.SidecarAddress = srv.URL
	h := mustNew(t, cfg)
	req := httptest.NewRequest("GET", "https://test.ch/", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := s.header.Get("X-Forwarded-Proto"); got != "https" {
		t.Errorf("X-Forwarded-Proto = %q", got)
	}
}

func TestUnixSocketSidecar(t *testing.T) {
	dir, err := os.MkdirTemp("", "ozt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	var got http.Header
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte("unix-ok"))
	})}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Service = "svc"
	cfg.SidecarAddress = "unix://" + sock
	h := mustNew(t, cfg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://test.ch/", nil))
	if rec.Body.String() != "unix-ok" || got.Get("X-Ziti-Service") != "svc" {
		t.Fatalf("unexpected: %d %q %v", rec.Code, rec.Body.String(), got)
	}
}

func TestSidecarDownGives502(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	cfg := CreateConfig()
	cfg.Service = "svc"
	cfg.SidecarAddress = "http://" + addr
	h := mustNew(t, cfg)
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://test.ch/", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), addr) {
		t.Error("body leaks internal address")
	}
}

func TestSidecar504PassedThrough(t *testing.T) {
	srv, _ := newSidecar(t, http.StatusGatewayTimeout)
	cfg := CreateConfig()
	cfg.Service = "svc"
	cfg.SidecarAddress = srv.URL
	h := mustNew(t, cfg)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://test.ch/", nil))
	if rec.Code != 504 || rec.Body.String() != "from-sidecar" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}
