package cache

import (
	"sync"
	"time"
)

type entry struct {
	pods      []string
	expiresAt time.Time
}

type Cache struct {
	mu    sync.RWMutex
	items map[string]entry
	ttl   time.Duration
}

func New(ttl time.Duration) *Cache {
	return &Cache{
		items: make(map[string]entry),
		ttl:   ttl,
	}
}

func (c *Cache) Get(key string) ([]string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.items[key]
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.pods, true
}

func (c *Cache) Set(key string, pods []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry{pods: pods, expiresAt: time.Now().Add(c.ttl)}
}

func (c *Cache) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}

// InvalidateByPod remove todas as entradas que contenham o podIP.
func (c *Cache) InvalidateByPod(podIP string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, e := range c.items {
		for _, ip := range e.pods {
			if ip == podIP {
				delete(c.items, key)
				break
			}
		}
	}
}
