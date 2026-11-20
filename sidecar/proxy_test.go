package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// fakeDialer connects to a local test server regardless of the service.
type fakeDialer struct {
	addr string
	err  error
	// block makes DialContext wait for ctx to end.
	block bool

	mu       sync.Mutex
	services []string
}

func (f *fakeDialer) DialContext(ctx context.Context, service string) (net.Conn, error) {
	f.mu.Lock()
	f.services = append(f.services, service)
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", f.addr)
}

type dest struct {
	Host, Method, URI, Body string
	Header                  http.Header
}

func startDest(t *testing.T, tlsServer bool) (*httptest.Server, *dest) {
	t.Helper()
	d := &dest{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		d.Host, d.Method, d.URI, d.Body, d.Header = r.Host, r.Method, r.RequestURI, string(b), r.Header.Clone()
		fmt.Fprint(w, "dest-ok")
	})
	var srv *httptest.Server
	if tlsServer {
		srv = httptest.NewTLSServer(h)
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	return srv, d
}

func serve(t *testing.T, h http.Handler) *httptest.Server {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func newHandler(d dialer, allowed []string, timeout time.Duration) *proxyHandler {
	return newProxyHandler(d, allowed, timeout, false, func() bool { return true })
}

func TestGetAndPostRoundTrip(t *testing.T) {
	ds, d := startDest(t, false)
	fd := &fakeDialer{addr: ds.Listener.Addr().String()}
	srv := serve(t, newHandler(fd, nil, time.Second))

	resp, body := do(t, srv, "GET", "/a/b?x=1&y=%2F", "", map[string]string{"X-Ziti-Service": "svc", "Host": "service.ziti"})
	if resp.StatusCode != 200 || body != "dest-ok" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	if d.URI != "/a/b?x=1&y=%2F" || d.Method != "GET" {
		t.Errorf("request altered: %+v", d)
	}
	resp, body = do(t, srv, "POST", "/p", "hello", map[string]string{"X-Ziti-Service": "svc"})
	if resp.StatusCode != 200 || d.Method != "POST" || d.Body != "hello" {
		t.Errorf("post failed: %d %+v", resp.StatusCode, d)
	}
	if len(fd.services) == 0 || fd.services[0] != "svc" {
		t.Errorf("dialed %v", fd.services)
	}
}

func TestControlHeadersStrippedForwardedKept(t *testing.T) {
	ds, d := startDest(t, false)
	srv := serve(t, newHandler(&fakeDialer{addr: ds.Listener.Addr().String()}, nil, time.Second))
	do(t, srv, "GET", "/", "", map[string]string{
		"X-Ziti-Service":         "svc",
		"X-Ziti-Upstream-Scheme": "http",
		"X-Ziti-Upstream-Host":   "x.internal",
		"X-Ziti-Whatever":        "1",
		"X-Forwarded-For":        "203.0.113.9",
		"X-Forwarded-Proto":      "https",
		"X-Forwarded-Host":       "service.ziti",
	})
	for k := range d.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-ziti-") {
			t.Errorf("%s reached destination", k)
		}
	}
	// The sidecar must not add its own hop to X-Forwarded-For.
	if got := d.Header.Values("X-Forwarded-For"); len(got) != 1 || got[0] != "203.0.113.9" {
		t.Errorf("X-Forwarded-For = %v", got)
	}
	if d.Header.Get("X-Forwarded-Proto") != "https" || d.Header.Get("X-Forwarded-Host") != "service.ziti" {
		t.Errorf("forwarded headers changed: %v", d.Header)
	}
}

func TestHostHeader(t *testing.T) {
	ds, d := startDest(t, false)
	srv := serve(t, newHandler(&fakeDialer{addr: ds.Listener.Addr().String()}, nil, time.Second))

	do(t, srv, "GET", "/", "", map[string]string{"X-Ziti-Service": "svc", "Host": "service.ziti"})
	if d.Host != "service.ziti" {
		t.Errorf("incoming host not preserved: %q", d.Host)
	}
	do(t, srv, "GET", "/", "", map[string]string{"X-Ziti-Service": "svc", "Host": "service.ziti", "X-Ziti-Upstream-Host": "backend.internal:8080"})
	if d.Host != "backend.internal:8080" {
		t.Errorf("override not applied: %q", d.Host)
	}
}

func TestHTTPSUpstream(t *testing.T) {
	ds, d := startDest(t, true)
	fd := &fakeDialer{addr: ds.Listener.Addr().String()}

	// Self-signed certificate: rejected when verification is on.
	srv := serve(t, newProxyHandler(fd, nil, time.Second, false, func() bool { return true }))
	resp, _ := do(t, srv, "GET", "/", "", map[string]string{"X-Ziti-Service": "svc", "X-Ziti-Upstream-Scheme": "https", "Host": "service.ziti"})
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("untrusted cert: status %d, want 502", resp.StatusCode)
	}

	srv2 := serve(t, newProxyHandler(fd, nil, time.Second, true, func() bool { return true }))
	resp, body := do(t, srv2, "GET", "/", "", map[string]string{"X-Ziti-Service": "svc", "X-Ziti-Upstream-Scheme": "https", "X-Ziti-Upstream-Host": "backend.internal:8443"})
	if resp.StatusCode != 200 || body != "dest-ok" || d.Host != "backend.internal:8443" {
		t.Errorf("https upstream failed: %d %q host=%q", resp.StatusCode, body, d.Host)
	}
}

func TestValidation(t *testing.T) {
	ds, _ := startDest(t, false)
	fd := &fakeDialer{addr: ds.Listener.Addr().String()}
	srv := serve(t, newHandler(fd, []string{"ok-svc", "other"}, time.Second))

	if r, _ := do(t, srv, "GET", "/", "", map[string]string{"X-Ziti-Service": "nope"}); r.StatusCode != 403 {
		t.Errorf("unlisted service: %d", r.StatusCode)
	}
	if r, _ := do(t, srv, "GET", "/", "", map[string]string{"X-Ziti-Service": "ok-svc"}); r.StatusCode != 200 {
		t.Errorf("listed service: %d", r.StatusCode)
	}
	if r, _ := do(t, srv, "GET", "/", "", map[string]string{"X-Ziti-Service": "ok-svc", "X-Ziti-Upstream-Scheme": "gopher"}); r.StatusCode != 400 {
		t.Errorf("bad scheme: %d", r.StatusCode)
	}
	if r, _ := do(t, srv, "GET", "/", "", nil); r.StatusCode != 400 {
		t.Errorf("missing service: %d", r.StatusCode)
	}
	if r, _ := do(t, srv, "POST", "/healthz", "", nil); r.StatusCode != 400 {
		t.Errorf("POST /healthz: %d", r.StatusCode)
	}
	if n := len(fd.services); n != 1 {
		t.Errorf("rejected requests must not dial, dials=%v", fd.services)
	}
}

func TestDialFailureAndTimeout(t *testing.T) {
	srv := serve(t, newHandler(&fakeDialer{err: fmt.Errorf("secret internal detail")}, nil, time.Second))
	r, body := do(t, srv, "GET", "/", "", map[string]string{"X-Ziti-Service": "svc"})
	if r.StatusCode != 502 {
		t.Errorf("dial failure: %d", r.StatusCode)
	}
	if strings.Contains(body, "secret") {
		t.Error("body leaks internal error")
	}

	srv2 := serve(t, newHandler(&fakeDialer{block: true}, nil, 100*time.Millisecond))
	start := time.Now()
	r, _ = do(t, srv2, "GET", "/", "", map[string]string{"X-Ziti-Service": "svc"})
	if r.StatusCode != 504 {
		t.Errorf("dial timeout: %d", r.StatusCode)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("timeout not honoured")
	}
}

func TestHealthz(t *testing.T) {
	ready := false
	srv := serve(t, newProxyHandler(&fakeDialer{}, nil, time.Second, false, func() bool { return ready }))
	if r, _ := do(t, srv, "GET", "/healthz", "", nil); r.StatusCode != 503 {
		t.Errorf("not ready: %d", r.StatusCode)
	}
	ready = true
	r, body := do(t, srv, "GET", "/healthz", "", nil)
	if r.StatusCode != 200 || body != "ok" {
		t.Errorf("ready: %d %q", r.StatusCode, body)
	}
}

func TestTransportCache(t *testing.T) {
	h := newHandler(&fakeDialer{}, nil, time.Second)
	a := h.transport("s")
	if a != h.transport("s") {
		t.Error("same service must reuse transport")
	}
	if a == h.transport("s2") {
		t.Error("different services must not share a transport")
	}
}

// Plain-HTTP requests for one service share one transport: sequential requests
// with different Host headers dial once, and the destination still sees each Host.
func TestPlainHTTPSharesPoolAcrossHosts(t *testing.T) {
	ds, d := startDest(t, false)
	fd := &fakeDialer{addr: ds.Listener.Addr().String()}
	srv := serve(t, newHandler(fd, nil, time.Second))
	for _, host := range []string{"a.service.ziti", "b.service.ziti", "c.service.ziti"} {
		do(t, srv, "GET", "/", "", map[string]string{"X-Ziti-Service": "svc", "Host": host})
		if d.Host != host {
			t.Errorf("destination Host = %q, want %q", d.Host, host)
		}
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.services) != 1 {
		t.Errorf("dials = %d, want 1", len(fd.services))
	}
}

// https keeps the TLS server name per upstream host.
func TestHTTPSSNIPerUpstreamHost(t *testing.T) {
	var mu sync.Mutex
	var snis []string
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "dest-ok")
	}))
	ts.TLS = &tls.Config{GetConfigForClient: func(hi *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		snis = append(snis, hi.ServerName)
		mu.Unlock()
		return nil, nil
	}}
	ts.StartTLS()
	t.Cleanup(ts.Close)

	fd := &fakeDialer{addr: ts.Listener.Addr().String()}
	srv := serve(t, newProxyHandler(fd, nil, time.Second, true, func() bool { return true }))
	for _, up := range []string{"backend-a.internal:8443", "backend-b.internal:8443"} {
		resp, body := do(t, srv, "GET", "/", "", map[string]string{"X-Ziti-Service": "svc", "X-Ziti-Upstream-Scheme": "https", "X-Ziti-Upstream-Host": up})
		if resp.StatusCode != 200 || body != "dest-ok" {
			t.Fatalf("https upstream %s: %d %q", up, resp.StatusCode, body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(snis, ","); got != "backend-a.internal,backend-b.internal" {
		t.Errorf("SNI sequence = %q", got)
	}
}

func TestTransportMapBounded(t *testing.T) {
	h := newHandler(&fakeDialer{}, nil, time.Second)
	n := 0
	for i := 0; i <= maxTransports; i++ {
		h.transport(fmt.Sprintf("svc-%d", i))
		h.mu.Lock()
		n = len(h.transports)
		h.mu.Unlock()
		if n > maxTransports {
			t.Fatalf("transport map holds %d entries, limit %d", n, maxTransports)
		}
	}
	// The overflowing insert reset the map, leaving only the newest transport.
	if n != 1 {
		t.Errorf("entries after overflow = %d, want 1", n)
	}
}

// A request cancelled by the client is not an error; a live-context
// cancellation still is.
func TestClientCancelNotLoggedAsError(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	h := newHandler(&fakeDialer{}, nil, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "http://service.ziti/", nil).WithContext(ctx)
	req.Header.Set(headerService, "svc")
	rec := httptest.NewRecorder()
	h.errorHandler(rec, req, context.Canceled)
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Errorf("client-cancelled request got a body: %q", rec.Body.String())
	}
	if strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("client cancellation logged at error level: %s", buf.String())
	}

	buf.Reset()
	live := httptest.NewRequest("GET", "http://service.ziti/", nil)
	live.Header.Set(headerService, "svc")
	rec = httptest.NewRecorder()
	h.errorHandler(rec, live, context.Canceled)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("live-context cancellation: status %d, want 502", rec.Code)
	}
	if !strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("live-context error not logged at error level: %s", buf.String())
	}
}

// Probes within the TTL reuse one controller check; after it expires the
// next probe checks again.
func TestHealthzCache(t *testing.T) {
	var calls atomic.Int32
	c := newReadyCache(func() bool { calls.Add(1); return true }, time.Hour)
	srv := serve(t, newProxyHandler(&fakeDialer{}, nil, time.Second, false, c.ready))
	for i := 0; i < 2; i++ {
		if r, body := do(t, srv, "GET", "/healthz", "", nil); r.StatusCode != 200 || body != "ok" {
			t.Fatalf("probe %d: %d %q", i, r.StatusCode, body)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("checks within TTL = %d, want 1", n)
	}

	var expired atomic.Int32
	c2 := newReadyCache(func() bool { expired.Add(1); return true }, time.Millisecond)
	c2.ready()
	time.Sleep(5 * time.Millisecond)
	c2.ready()
	if n := expired.Load(); n != 2 {
		t.Fatalf("checks after TTL = %d, want 2", n)
	}
}

// startWSDest accepts an HTTP upgrade and echoes bytes back, uppercased.
func startWSDest(t *testing.T) *httptest.Server {
	return serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "no upgrade", 400)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		sc := bufio.NewScanner(rw)
		for sc.Scan() {
			fmt.Fprintf(rw, "%s\n", strings.ToUpper(sc.Text()))
			rw.Flush()
		}
	}))
}

func upgradeRoundTrip(t *testing.T, addr, host string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", host)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, msg := range []string{"hello", "again"} {
		fmt.Fprintf(conn, "%s\n", msg)
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) != strings.ToUpper(msg) {
			t.Fatalf("echo = %q", line)
		}
	}
}

func TestWebSocketThroughSidecar(t *testing.T) {
	ws := startWSDest(t)
	srv := serve(t, newHandler(&fakeDialer{addr: ws.Listener.Addr().String()}, nil, time.Second))
	// Talk to the sidecar directly, setting the control header ourselves.
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: service.ziti\r\nX-Ziti-Service: svc\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	fmt.Fprint(conn, "ping\n")
	if line, _ := br.ReadString('\n'); strings.TrimSpace(line) != "PING" {
		t.Fatalf("echo = %q", line)
	}
}

// pluginHelper is a tiny program that wraps the real plugin source
// (../openziti.go, copied unmodified) in an HTTP server.
const pluginHelperMain = `package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"

	plugin "example.com/pluginharness/plugin"
)

func main() {
	cfg := plugin.CreateConfig()
	cfg.Service = "ws-svc"
	cfg.SidecarAddress = os.Args[1]
	h, err := plugin.New(context.Background(), http.NotFoundHandler(), cfg, "harness")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(l.Addr().String())
	_ = http.Serve(l, h)
}
`

func TestWebSocketThroughPluginAndSidecar(t *testing.T) {
	src, err := os.ReadFile("../openziti.go")
	if err != nil {
		t.Skipf("plugin source not available: %v", err)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not available")
	}
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "plugin"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/pluginharness\n\ngo 1.22\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "plugin", "openziti.go"), src, 0o644))
	must(os.WriteFile(filepath.Join(dir, "main.go"), []byte(pluginHelperMain), 0o644))
	bin := filepath.Join(dir, "harness")
	build := exec.Command(goBin, "build", "-o", bin, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin harness: %v\n%s", err, out)
	}

	ws := startWSDest(t)
	fd := &fakeDialer{addr: ws.Listener.Addr().String()}
	sidecar := serve(t, newHandler(fd, []string{"ws-svc"}, time.Second))

	cmd := exec.Command(bin, "http://"+sidecar.Listener.Addr().String())
	stdout, err := cmd.StdoutPipe()
	must(err)
	must(cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr, err := bufio.NewReader(stdout).ReadString('\n')
	must(err)

	upgradeRoundTrip(t, strings.TrimSpace(addr), "service.ziti")
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.services) == 0 || fd.services[0] != "ws-svc" {
		t.Errorf("sidecar dialed %v", fd.services)
	}
}

// BenchmarkProxyGET sends parallel GET requests through a real HTTP server in
// front of the proxy handler. Requests rotate over 8 client Host headers for one
// Ziti service, so the number of upstream transports and dials is visible.
func BenchmarkProxyGET(b *testing.B) {
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("dest-ok"))
	}))
	defer dest.Close()
	fd := &fakeDialer{addr: dest.Listener.Addr().String()}
	front := httptest.NewServer(newHandler(fd, nil, 5*time.Second))
	defer front.Close()

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}
	defer client.CloseIdleConnections()
	hosts := make([]string, 8)
	for i := range hosts {
		hosts[i] = string(rune('a'+i)) + ".service.ziti"
	}

	var n atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, err := http.NewRequest("GET", front.URL+"/", nil)
			if err != nil {
				b.Error(err)
				return
			}
			req.Host = hosts[n.Add(1)%uint64(len(hosts))]
			req.Header.Set(headerService, "svc")
			resp, err := client.Do(req)
			if err != nil {
				b.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				b.Errorf("status %d", resp.StatusCode)
				return
			}
		}
	})
	b.StopTimer()
	fd.mu.Lock()
	dials := len(fd.services)
	fd.mu.Unlock()
	b.ReportMetric(float64(dials), "dials")
}
