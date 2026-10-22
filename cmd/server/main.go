// Command tls-secret-gateway exposes a secure HTTP gateway for Kubernetes Secrets of
// type "kubernetes.io/tls" across all namespaces.
//
// It runs with a ServiceAccount that has cluster-wide Secret read access, but
// the served API only ever returns TLS-type secrets, preventing callers from
// reading unrelated sensitive secrets.
package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/example/tls-secret-gateway/internal/server"
)

func main() {
	klog.InitFlags(nil)
	defer klog.Flush()

	cfg, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("failed to build in-cluster config: %v", err)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("failed to create kubernetes client: %v", err)
	}

	authEnabled, _ := strconv.ParseBool(envOrDefault("AUTH_ENABLED", "true"))
	allowed := splitCSV(os.Getenv("ALLOWED_SERVICE_ACCOUNTS"))
	resync := parseDuration("RESYNC_PERIOD", 10*time.Minute)
	listenAddr := envOrDefault("LISTEN_ADDR", ":8080")

	runCfg := server.Config{
		ListenAddr:   listenAddr,
		ResyncPeriod: resync,
		AuthEnabled:  authEnabled,
		AllowedSAs:   allowed,
	}

	ctx := context.Background()
	if err := server.Run(ctx, client, runCfg); err != nil {
		klog.Fatalf("server exited with error: %v", err)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		klog.Warningf("invalid %s %q, using default %s", key, v, def)
		return def
	}
	return d
}
