package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"controller/internal/store"
)

type podCounter struct {
	count int64
}

type Metrics struct {
	store    *store.Store
	counters sync.Map
}

func New(s *store.Store) *Metrics {
	return &Metrics{store: s}
}

func (m *Metrics) IncrementPodRequest(podIP string) {
	v, _ := m.counters.LoadOrStore(podIP, &podCounter{})
	atomic.AddInt64(&v.(*podCounter).count, 1)
}

// StartLoop a cada segundo faz swap dos contadores e persiste no Redis como rps:{podIP}.
func (m *Metrics) StartLoop() {
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			ctx := context.Background()
			m.counters.Range(func(k, v interface{}) bool {
				podIP := k.(string)
				counter := v.(*podCounter)
				rps := atomic.SwapInt64(&counter.count, 0)
				m.store.SetRPS(ctx, podIP, rps)
				return true
			})
		}
	}()
}

func (m *Metrics) HandlePodMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	result, err := m.store.GetAllRPS(ctx)
	if err != nil {
		http.Error(w, "redis error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
