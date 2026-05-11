// Package main implements an HTTP caching proxy that stores upstream responses
// in S3 and serves cache hits via presigned URL redirects.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/coreos/go-systemd/v22/activation"
	"github.com/coreos/go-systemd/v22/daemon"

	"github.com/thomasdesr/external-mirror-cache/internal/errorutil"
	"github.com/thomasdesr/external-mirror-cache/internal/reqlog"
)

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

var (
	bucket = flag.String("bucket", envDefault("MIRROR_CACHE_BUCKET", ""), "S3 bucket for cached responses (env: MIRROR_CACHE_BUCKET)")
	prefix = flag.String("prefix", envDefault("MIRROR_CACHE_PREFIX", "cache"), "S3 key prefix (env: MIRROR_CACHE_PREFIX)")
	listen = flag.String("listen", ":8443", "listen address (ignored under socket activation)")

	egressProxy = flag.String("egress-proxy", "", "HTTP CONNECT proxy for upstream requests (e.g. http://127.0.0.1:4750)")

	logLevel = flag.String("log-level", envDefault("MIRROR_CACHE_LOG_LEVEL", "info"),
		"log level: debug, info, warn, error (env: MIRROR_CACHE_LOG_LEVEL)")

	staleOnConnectionError = flag.Bool("stale-on-connection-error", true, "serve stale content on connection errors (timeouts, DNS failures)")
	staleOn5xx             = flag.Bool("stale-on-5xx", true, "serve stale content on upstream 5xx errors")
	staleOnAnyError        = flag.Bool("stale-on-any-error", false, "serve stale content on any upstream error")

	clientTimeoutStr = flag.String(
		"client-timeout",
		envDefault("MIRROR_CACHE_CLIENT_TIMEOUT", "120s"),
		"overall http.Client.Timeout; bounds total upstream request time including body streaming; 0 disables (env: MIRROR_CACHE_CLIENT_TIMEOUT)",
	)
	responseHeaderTimeoutStr = flag.String(
		"response-header-timeout",
		envDefault("MIRROR_CACHE_RESPONSE_HEADER_TIMEOUT", "5s"),
		"max wait for upstream response headers; on timeout conditional fetches"+
			" fall back to stale, cache-miss returns 502; 0 disables"+
			" (env: MIRROR_CACHE_RESPONSE_HEADER_TIMEOUT)",
	)
)

func main() {
	flag.Parse()

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

var (
	errBucketRequired   = errors.New("--bucket or MIRROR_CACHE_BUCKET is required")
	errNegativeDuration = errors.New("duration must be zero or positive")
)

func run() error {
	if err := setupLogging(*logLevel); err != nil {
		return err
	}

	if *bucket == "" {
		return errBucketRequired
	}

	clientTimeout, responseHeaderTimeout, err := parseTimeoutFlags()
	if err != nil {
		return err
	}

	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithEC2IMDSRegion())
	if err != nil {
		return errorutil.Wrap(err, "load AWS config")
	}

	s3c := s3.NewFromConfig(cfg)

	client, ociTransport, err := newUpstreamClient(clientTimeout, responseHeaderTimeout)
	if err != nil {
		return err
	}
	defer ociTransport.Close()

	s3Cache := &s3HTTPCache{
		s3c:    s3c,
		s3pc:   s3.NewPresignClient(s3c),
		s3u:    transfermanager.New(s3c),
		bucket: *bucket,
		prefix: *prefix,
	}

	handler := &cacheMiddleware{
		cache:  s3Cache,
		client: client,
		fallback: FallbackPolicy{
			OnConnectionError: *staleOnConnectionError,
			On5xx:             *staleOn5xx,
			OnAnyError:        *staleOnAnyError,
		},
		keyFunc: ociAwareKeyFunc,
	}

	ln, err := getListener(*listen)
	if err != nil {
		return errorutil.Wrap(err, "get listener")
	}

	srv := &http.Server{
		Handler:           reqlog.Middleware(handler),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := runServer(ctx, srv, ln); err != nil {
		return errorutil.Wrap(err, "server")
	}

	slog.Info("server stopped")

	return nil
}

// newUpstreamClient builds the http.Client used for upstream fetches. Only
// this client goes through the egress proxy; AWS SDK traffic (S3, IMDS) uses
// the default transport directly. Caller must Close the returned transport.
func newUpstreamClient(clientTimeout, responseHeaderTimeout time.Duration) (*http.Client, *ociAuthTransport, error) {
	transport := http.DefaultTransport.(*http.Transport) //nolint:forcetypeassert // intentional panic

	transport = transport.Clone()
	transport.Proxy = nil

	if *egressProxy != "" {
		proxyURL, err := url.Parse(*egressProxy)
		if err != nil {
			return nil, nil, errorutil.Wrap(err, "invalid --egress-proxy URL")
		}

		transport.Proxy = http.ProxyURL(proxyURL)

		slog.Info("upstream requests proxied via egress proxy", "proxy", *egressProxy)
	}

	transport.DialContext = (&net.Dialer{
		Timeout: 10 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	transport.IdleConnTimeout = 90 * time.Second

	ociTransport := newOCIAuthTransport(transport)

	client := &http.Client{
		Transport: ociTransport,
		Timeout:   clientTimeout,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			reqlog.FromContext(req.Context()).Debug("following redirect", "url", req.URL.String())

			return nil
		},
	}

	return client, ociTransport, nil
}

func parseTimeoutFlags() (client, responseHeader time.Duration, err error) {
	client, err = parseDuration(*clientTimeoutStr, "client-timeout")
	if err != nil {
		return 0, 0, err
	}

	responseHeader, err = parseDuration(*responseHeaderTimeoutStr, "response-header-timeout")
	if err != nil {
		return 0, 0, err
	}

	return client, responseHeader, nil
}

func parseDuration(s, name string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, errorutil.Wrapf(err, "invalid --%s value %q", name, s)
	}

	if d < 0 {
		return 0, errorutil.Wrapf(errNegativeDuration, "invalid --%s value %q", name, s)
	}

	return d, nil
}

func setupLogging(levelStr string) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(levelStr)); err != nil {
		return errorutil.Wrapf(err, "invalid log level %q", levelStr)
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if isTTY(os.Stderr) {
		handler = slog.NewTextHandler(os.Stderr, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}

	slog.SetDefault(slog.New(handler))

	slog.Info("starting mirror-cache",
		"bucket", *bucket,
		"prefix", *prefix,
		"listen", *listen,
		"log_level", level.String(),
	)

	return nil
}

func getListener(addr string) (net.Listener, error) {
	listeners, err := activation.Listeners()
	if err != nil {
		return nil, errorutil.Wrap(err, "socket activation")
	}

	if len(listeners) > 0 {
		slog.Info("using socket-activated listener")

		return listeners[0], nil
	}

	slog.Info("listening", "addr", addr)

	lc := net.ListenConfig{}

	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, errorutil.Wrapf(err, "listen %s", addr)
	}

	return ln, nil
}

func runServer(ctx context.Context, srv *http.Server, ln net.Listener) error {
	serverErrors := make(chan error, 1)

	go func() {
		serverErrors <- srv.Serve(ln)
	}()

	_, _ = daemon.SdNotify(false, daemon.SdNotifyReady)

	select {
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("received shutdown signal, starting graceful shutdown")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil { //nolint:contextcheck // fresh context after signal
			slog.Error("server shutdown error", "error", err)
		}
	}

	return nil
}

func isTTY(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}
