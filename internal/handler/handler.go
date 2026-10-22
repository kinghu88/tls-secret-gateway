// Package handler implements the HTTP API for the TLS Secret proxy.
package handler

import (
	"encoding/json"
	"net/http"

	corev1 "k8s.io/api/core/v1"

	"github.com/example/tls-secret-gateway/internal/cache"
)

// Handler serves TLS Secret read endpoints backed by a TLSCache.
type Handler struct {
	cache *cache.TLSCache
}

// New creates a Handler.
func New(c *cache.TLSCache) *Handler {
	return &Handler{cache: c}
}

// secretView is the JSON representation returned to clients.
// Only non-sensitive metadata plus the cert/key data (base64-encoded, as stored).
type secretView struct {
	Namespace   string            `json:"namespace"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Data        map[string][]byte `json:"data,omitempty"` // tls.crt / tls.key base64
	ResourceVersion string        `json:"resourceVersion"`
}

func toView(s *corev1.Secret) secretView {
	return secretView{
		Namespace:       s.Namespace,
		Name:            s.Name,
		Type:            string(s.Type),
		Labels:          s.Labels,
		Annotations:     s.Annotations,
		Data:            s.Data,
		ResourceVersion: s.ResourceVersion,
	}
}

// Register wires all routes onto mux using Go 1.22+ ServeMux patterns.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/tls-secrets", h.list)
	mux.HandleFunc("GET /v1/tls-secrets/{namespace}/{name}", h.get)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	items := h.cache.List(ns)
	out := make([]secretView, 0, len(items))
	for _, s := range items {
		out = append(out, toView(s))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items": out,
		"count": len(out),
	})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	if ns == "" || name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "namespace and name are required"})
		return
	}
	s := h.cache.Get(ns, name)
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, toView(s))
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
