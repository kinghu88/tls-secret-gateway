// Package auth provides optional bearer-token authentication for the proxy.
// When enabled, callers must present a Kubernetes ServiceAccount token in the
// Authorization header. The token is validated via the TokenReview API, and
// access can be further restricted to a configurable allowlist of
// "namespace:serviceaccount" pairs.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// Authenticator validates bearer tokens using TokenReview.
type Authenticator struct {
	client   kubernetes.Interface
	allowed  map[string]struct{} // "namespace:name" pairs; empty means allow any authenticated
	enabled  bool
	mu       sync.RWMutex
}

// New creates an Authenticator. If enabled is false, Authenticate always succeeds.
// allowed is a list of "namespace:name" SA identifiers; empty/nil allows any
// authenticated SA.
func New(client kubernetes.Interface, enabled bool, allowed []string) *Authenticator {
	a := &Authenticator{client: client, enabled: enabled}
	a.setAllowed(allowed)
	return a
}

func (a *Authenticator) setAllowed(allowed []string) {
	m := make(map[string]struct{}, len(allowed))
	for _, s := range allowed {
		s = strings.TrimSpace(s)
		if s != "" {
			m[s] = struct{}{}
		}
	}
	a.mu.Lock()
	a.allowed = m
	a.mu.Unlock()
}

// Enabled reports whether authentication is active.
func (a *Authenticator) Enabled() bool { return a.enabled }

// Authenticate extracts the bearer token and validates it. Returns the
// authenticated username (e.g. "system:serviceaccount:ns:name") on success.
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (string, error) {
	if !a.enabled {
		return "", nil
	}

	token := bearerToken(r)
	if token == "" {
		return "", fmt.Errorf("missing bearer token")
	}

	resp, err := a.client.AuthenticationV1().TokenReviews().Create(ctx,
		&authenticationv1.TokenReview{
			Spec: authenticationv1.TokenReviewSpec{Token: token},
		}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("token review failed: %w", err)
	}

	if !resp.Status.Authenticated {
		return "", fmt.Errorf("token not authenticated: %s", resp.Status.Error)
	}

	user := resp.Status.User.Username
	if !a.allowAll() {
		// username format: system:serviceaccount:<namespace>:<name>
		parts := strings.Split(user, ":")
		if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
			return "", fmt.Errorf("unexpected user identity: %s", user)
		}
		key := parts[2] + ":" + parts[3]
		a.mu.RLock()
		_, ok := a.allowed[key]
		a.mu.RUnlock()
		if !ok {
			return "", fmt.Errorf("service account %q not in allowlist", key)
		}
	}

	return user, nil
}

func (a *Authenticator) allowAll() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.allowed) == 0
}

// Middleware returns an http.Handler wrapper that enforces authentication.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := a.Authenticate(r.Context(), r)
		if err != nil {
			klog.Warningf("auth failed from %s: %v", r.RemoteAddr, err)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		if user != "" {
			klog.V(4).Infof("authenticated request from %s", user)
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
