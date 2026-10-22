// Package cache provides an in-memory cache of TLS-type Secrets across all namespaces.
// It uses a SharedInformer to watch Secret events and keeps only secrets of type
// "kubernetes.io/tls", so the HTTP layer never exposes non-TLS secrets to callers.
package cache

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// TLSCache holds a thread-safe view of all TLS Secrets in the cluster.
type TLSCache struct {
	client   kubernetes.Interface
	stopCh   chan struct{}
	started  bool
	mu       sync.RWMutex
	items    map[string]*corev1.Secret // key: namespace/name
	synced   bool
	resync   time.Duration
}

// New creates a TLSCache. resync controls how often the informer re-lists
// from the API server; 0 disables periodic resync.
func New(client kubernetes.Interface, resync time.Duration) *TLSCache {
	if resync <= 0 {
		resync = 0
	}
	return &TLSCache{
		client: client,
		items:  make(map[string]*corev1.Secret),
		resync: resync,
	}
}

func keyOf(ns, name string) string { return ns + "/" + name }

// Start launches the informer and blocks until the cache is synced or ctx is done.
func (c *TLSCache) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return fmt.Errorf("cache already started")
	}
	c.started = true
	c.stopCh = make(chan struct{})
	c.mu.Unlock()

	factory := informers.NewSharedInformerFactory(c.client, c.resync)
	informer := factory.Core().V1().Secrets().Informer()

	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			c.upsert(obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			c.upsert(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			c.remove(obj)
		},
	}); err != nil {
		return fmt.Errorf("add event handler: %w", err)
	}

	factory.Start(c.stopCh)

	klog.Info("waiting for TLS secret cache to sync")
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return fmt.Errorf("cache sync failed or context cancelled")
	}

	c.mu.Lock()
	c.synced = true
	c.mu.Unlock()
	klog.Infof("TLS secret cache synced with %d items", len(c.items))
	return nil
}

// Stop signals the informer to shut down.
func (c *TLSCache) Stop() {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.stopCh != nil {
		close(c.stopCh)
	}
}

func (c *TLSCache) upsert(obj interface{}) {
	s, ok := obj.(*corev1.Secret)
	if !ok {
		return
	}
	if s.Type != corev1.SecretTypeTLS {
		return
	}
	c.mu.Lock()
	c.items[keyOf(s.Namespace, s.Name)] = s
	c.mu.Unlock()
}

func (c *TLSCache) remove(obj interface{}) {
	s, ok := obj.(*corev1.Secret)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		s, ok = tombstone.Obj.(*corev1.Secret)
		if !ok {
			return
		}
	}
	c.mu.Lock()
	delete(c.items, keyOf(s.Namespace, s.Name))
	c.mu.Unlock()
}

// Synced reports whether the informer has completed its initial list.
func (c *TLSCache) Synced() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.synced
}

// List returns all cached TLS Secrets, optionally filtered by namespace.
// An empty namespace returns secrets from all namespaces.
func (c *TLSCache) List(namespace string) []*corev1.Secret {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*corev1.Secret, 0, len(c.items))
	for _, s := range c.items {
		if namespace == "" || s.Namespace == namespace {
			out = append(out, s)
		}
	}
	return out
}

// Get returns a single TLS Secret by namespace/name, or nil if not cached.
func (c *TLSCache) Get(namespace, name string) *corev1.Secret {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.items[keyOf(namespace, name)]
}

// ListBySelector is a convenience that applies a label selector to cached items.
func (c *TLSCache) ListBySelector(namespace string, sel labels.Selector) []*corev1.Secret {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*corev1.Secret, 0)
	for _, s := range c.items {
		if namespace != "" && s.Namespace != namespace {
			continue
		}
		if sel != nil && !sel.Matches(labels.Set(s.Labels)) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Count returns the number of cached TLS Secrets.
func (c *TLSCache) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
