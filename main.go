// Package main implements an HTTP caching proxy that stores upstream responses
// in S3 and serves cache hits via presigned URL redirects.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
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

	staleOnConnectionError = flag.Bool("stale-on-connection-error", true, "serve stale content on connection errors (timeouts, DNS failures)")
	staleOn5xx             = flag.Bool("stale-on-5xx", true, "serve stale content on upstream 5xx errors")
	staleOnAnyError        = flag.Bool("stale-on-any-error", false, "serve stale content on any upstream error")
)

func main() {
	flag.Parse()

	if err := run(); err != nil {
		log.Fatal(err)
	}
}

var errBucketRequired = errors.New("--bucket or MIRROR_CACHE_BUCKET is required")

func run() error {
	if *bucket == "" {
		return errBucketRequired
	}

	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithEC2IMDSRegion())
	if err != nil {
		return errorutil.Wrap(err, "load AWS config")
	}

	s3c := s3.NewFromConfig(cfg)

	// Upstream fetch transport. Only this client goes through the egress
	// proxy; AWS SDK traffic (S3, IMDS) uses the default transport directly.
	transport := http.DefaultTransport.(*http.Transport) //nolint:forcetypeassert // intentional panic

	transport = transport.Clone()
	transport.Proxy = nil

	if *egressProxy != "" {
		proxyURL, err := url.Parse(*egressProxy)
		if err != nil {
			return errorutil.Wrap(err, "invalid --egress-proxy URL")
		}

		transport.Proxy = http.ProxyURL(proxyURL)

		log.Printf("Upstream requests proxied via %s", *egressProxy)
	}

	transport.DialContext = (&net.Dialer{
		Timeout: 10 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.IdleConnTimeout = 90 * time.Second

	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Minute,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			log.Println("Following redirect:", req.URL)

			return nil
		},
	}

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
	}

	ln, err := getListener(*listen)
	if err != nil {
		return errorutil.Wrap(err, "get listener")
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := runServer(ctx, srv, ln); err != nil {
		return errorutil.Wrap(err, "server")
	}

	log.Println("Server stopped")

	return nil
}

func getListener(addr string) (net.Listener, error) {
	listeners, err := activation.Listeners()
	if err != nil {
		return nil, errorutil.Wrap(err, "socket activation")
	}

	if len(listeners) > 0 {
		log.Println("Using socket-activated listener")

		return listeners[0], nil
	}

	log.Printf("Listening on %s", addr)

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
		log.Println("Received shutdown signal, starting graceful shutdown")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil { //nolint:contextcheck // fresh context after signal
			log.Printf("Server shutdown error: %v", err)
		}
	}

	return nil
}
