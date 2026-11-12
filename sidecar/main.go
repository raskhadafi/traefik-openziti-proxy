// Command sidecar holds an OpenZiti identity and dials Ziti services on behalf
// of the Traefik plugin.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/openziti/edge-api/rest_model"
	"github.com/openziti/sdk-golang/ziti"
)

type config struct {
	listen       string
	identityFile string
	jwtFile      string
	keyAlg       string
	allowed      []string
	dialTimeout  time.Duration
	insecureSkip bool
}

func envOr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

func parseConfig(args []string) (*config, error) {
	fs := flag.NewFlagSet("sidecar", flag.ContinueOnError)
	listen := fs.String("listen", envOr("ZITI_SIDECAR_LISTEN", "127.0.0.1:8900"), "host:port or unix:///path/to.sock [ZITI_SIDECAR_LISTEN]")
	identity := fs.String("identity-file", envOr("ZITI_IDENTITY_FILE", ""), "where the enrolled identity JSON is stored (required) [ZITI_IDENTITY_FILE]")
	jwt := fs.String("jwt-file", envOr("ZITI_JWT_FILE", ""), "one-time enrollment JWT, needed only when the identity file does not exist [ZITI_JWT_FILE]")
	keyAlg := fs.String("key-alg", envOr("ZITI_KEY_ALG", "EC"), "EC or RSA [ZITI_KEY_ALG]")
	allowed := fs.String("allowed-services", envOr("ZITI_ALLOWED_SERVICES", ""), "comma-separated service allowlist, empty = all [ZITI_ALLOWED_SERVICES]")
	dialTimeout := fs.String("dial-timeout", envOr("ZITI_DIAL_TIMEOUT", "15s"), "Go duration [ZITI_DIAL_TIMEOUT]")
	insecure := fs.String("insecure-skip-verify", envOr("ZITI_INSECURE_SKIP_VERIFY", "false"), "skip TLS verification for https upstreams [ZITI_INSECURE_SKIP_VERIFY]")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	c := &config{listen: *listen, identityFile: *identity, jwtFile: *jwt, keyAlg: *keyAlg}
	if c.identityFile == "" {
		return nil, errors.New("--identity-file (ZITI_IDENTITY_FILE) is required")
	}
	if c.keyAlg != "EC" && c.keyAlg != "RSA" {
		return nil, fmt.Errorf("--key-alg must be EC or RSA, got %q", c.keyAlg)
	}
	d, err := time.ParseDuration(*dialTimeout)
	if err != nil || d <= 0 {
		return nil, fmt.Errorf("--dial-timeout %q is not a positive Go duration", *dialTimeout)
	}
	c.dialTimeout = d
	b, err := strconv.ParseBool(*insecure)
	if err != nil {
		return nil, fmt.Errorf("--insecure-skip-verify %q is not a boolean", *insecure)
	}
	c.insecureSkip = b
	for _, s := range strings.Split(*allowed, ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.allowed = append(c.allowed, s)
		}
	}
	if strings.HasPrefix(c.listen, "unix://") {
		if p := strings.TrimPrefix(c.listen, "unix://"); !strings.HasPrefix(p, "/") || len(p) < 2 {
			return nil, fmt.Errorf("--listen %q: unix address must be unix:///absolute/path", c.listen)
		}
	} else if _, _, err := net.SplitHostPort(c.listen); err != nil {
		return nil, fmt.Errorf("--listen %q: %w", c.listen, err)
	}
	return c, nil
}

func listen(addr string) (net.Listener, error) {
	if !strings.HasPrefix(addr, "unix://") {
		return net.Listen("tcp", addr)
	}
	path := strings.TrimPrefix(addr, "unix://")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// zitiDialer adapts ziti.Context to the dialer interface.
type zitiDialer struct{ ctx ziti.Context }

func (z zitiDialer) DialContext(ctx context.Context, service string) (net.Conn, error) {
	return z.ctx.DialContext(ctx, service)
}

func warnUndialable(zctx ziti.Context, allowed []string) {
	if len(allowed) == 0 {
		return
	}
	services, err := zctx.GetServices()
	if err != nil {
		slog.Warn("could not list services", "error", err)
		return
	}
	dialable := map[string]bool{}
	for _, s := range services {
		if s.Name == nil {
			continue
		}
		for _, p := range s.Permissions {
			if p == rest_model.DialBindDial {
				dialable[*s.Name] = true
			}
		}
	}
	for _, name := range allowed {
		if !dialable[name] {
			slog.Warn("service not dialable", "service", name)
		}
	}
}

func run() error {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		return err
	}
	zcfg, err := loadOrEnroll(cfg.identityFile, cfg.jwtFile, cfg.keyAlg, enrollWithSDK)
	if err != nil {
		return err
	}
	zctx, err := ziti.NewContext(zcfg)
	if err != nil {
		return fmt.Errorf("create ziti context: %w", err)
	}
	defer zctx.Close()
	if err := zctx.Authenticate(); err != nil {
		return fmt.Errorf("authenticate with the Ziti controller: %w", err)
	}
	slog.Info("authenticated")
	warnUndialable(zctx, cfg.allowed)

	h := newProxyHandler(zitiDialer{zctx}, cfg.allowed, cfg.dialTimeout, cfg.insecureSkip, func() bool {
		_, err := zctx.GetServices()
		return err == nil
	})
	l, err := listen(cfg.listen)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", cfg.listen, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	slog.Info("listening", "address", cfg.listen)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case err := <-errc:
		return err
	case <-sig:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(sctx)
	h.closeIdle()
	return err
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}
