// Package server wires the cache, handler and auth into an HTTP server with
// graceful shutdown, health/readiness probes and structured logging.
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/example/tls-secret-gateway/internal/auth"
	"github.com/example/tls-secret-gateway/internal/cache"
	"github.com/example/tls-secret-gateway/internal/handler"
)

// Config holds runtime configuration.
type Config struct {
	ListenAddr        string
	ResyncPeriod      time.Duration
	AuthEnabled       bool
	AllowedSAs        []string
	ShutdownTimeout   time.Duration
}

// Run starts the proxy and blocks until a shutdown signal is received.
func Run(ctx context.Context, client kubernetes.Interface, cfg Config) error {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 15 * time.Second
	}

	tlsCache := cache.New(client, cfg.ResyncPeriod)
	if err := tlsCache.Start(ctx); err != nil {
		return fmt.Errorf("start cache: %w", err)
	}
	defer tlsCache.Stop()

	mux := http.NewServeMux()

	// Health/readiness are intentionally NOT behind auth so kubelet can probe.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !tlsCache.Synced() {
			http.Error(w, "cache not synced", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "ready (cached=%d)", tlsCache.Count())
	})

	h := handler.New(tlsCache)
	h.Register(mux)

	// Auth middleware wraps only the API routes; health checks stay public.
	authn := auth.New(client, cfg.AuthEnabled, cfg.AllowedSAs)
	var root http.Handler = mux
	if authn.Enabled() {
		klog.Info("authentication enabled (TokenReview)")
		root = skipPaths(authn.Middleware, mux, "/healthz", "/readyz")
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		klog.Infof("TLS secret proxy listening on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			klog.Errorf("server error: %v", err)
		}
	}()

	// Wait for shutdown signal or context cancellation.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		klog.Infof("received signal %s, shutting down", sig)
	case <-ctx.Done():
		klog.Info("context cancelled, shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		klog.Errorf("graceful shutdown failed: %v", err)
		return err
	}
	klog.Info("server stopped cleanly")
	return nil
}

// skipPaths wraps next so that requests whose path is in skip are served
// directly without going through wrap.
func skipPaths(wrap func(http.Handler) http.Handler, next http.Handler, skip ...string) http.Handler {
	skipSet := make(map[string]struct{}, len(skip))
	for _, p := range skip {
		skipSet[p] = struct{}{}
	}
	wrapped := wrap(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := skipSet[r.URL.Path]; ok {
			next.ServeHTTP(w, r)
			return
		}
		wrapped.ServeHTTP(w, r)
	})
}
