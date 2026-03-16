package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"controller/internal/store"
)

type podCounter struct {
	count int64
}

// PodMetricResult é o que o endpoint /pods/metrics retorna por pod.
type PodMetricResult struct {
	CurrentRequests int64   `json:"current_requests"` // acumulado desde o último tick (in-memory)
	LastRPS         float64 `json:"last_rps"`          // RPS gravado no segundo anterior (Redis)
}

type Metrics struct {
	store    *store.Store
	counters sync.Map
}

func New(s *store.Store) *Metrics {
	return &Metrics{store: s}
}

// IncrementPodRequest incrementa o contador do pod. Remove o prefixo "http://" do IP.
func (m *Metrics) IncrementPodRequest(podIP string) {
	podIP = strings.TrimPrefix(podIP, "http://")
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

// HandlePodMetrics combina os contadores in-memory (acumulado atual) com os
// últimos valores de RPS gravados no Redis, para nunca retornar em branco.
func (m *Metrics) HandlePodMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()

	result := make(map[string]PodMetricResult)

	// 1. popula com os últimos RPS do Redis
	redisRPS, err := m.store.GetAllRPS(ctx)
	if err == nil {
		for podIP, rps := range redisRPS {
			result[podIP] = PodMetricResult{LastRPS: rps}
		}
	}

	// 2. sobrepõe/complementa com os contadores in-memory (requisições desde o último tick)
	m.counters.Range(func(k, v interface{}) bool {
		podIP := k.(string)
		current := atomic.LoadInt64(&v.(*podCounter).count)
		entry := result[podIP]
		entry.CurrentRequests = current
		result[podIP] = entry
		return true
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
