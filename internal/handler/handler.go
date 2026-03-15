package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"controller/internal/k8s"
	"controller/internal/metrics"
	"controller/internal/router"
	"controller/internal/store"
)

type Handler struct {
	router  *router.Router
	store   *store.Store
	metrics *metrics.Metrics
	watcher *k8s.Watcher
}

func New(r *router.Router, s *store.Store, m *metrics.Metrics, w *k8s.Watcher) *Handler {
	return &Handler{router: r, store: s, metrics: m, watcher: w}
}

func (h *Handler) Lookup(w http.ResponseWriter, r *http.Request) {
	artifact := r.URL.Query().Get("artifact")
	if artifact == "" {
		http.Error(w, "missing artifact", http.StatusBadRequest)
		return
	}

	ctx := context.Background()
	pods, err := h.store.GetPodsByArtifact(ctx, artifact)
	if err != nil || len(pods) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	total := h.store.GetTotalPods(ctx)
	var ratio float64
	if total > 0 {
		ratio = float64(len(pods)) / float64(total)
	} else {
		ratio = 1.0
	}

	w.Header().Set("x-spread-ratio", fmt.Sprintf("%.2f", ratio))
	w.Header().Set("x-spread-target", fmt.Sprintf("%.2f", h.store.SpreadTarget()))
	w.Write([]byte(pods[rand.Intn(len(pods))]))
}

func (h *Handler) Submit(w http.ResponseWriter, r *http.Request) {
	operator := r.Header.Get("operator")
	target, _, shouldRecord := h.router.ChooseTarget(r.Context(), operator)
	h.metrics.IncrementPodRequest(target)

	targetURL := target + r.URL.Path

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "proxy error", http.StatusBadGateway)
		return
	}
	proxyReq.Header = r.Header.Clone()

	resp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	ctx := context.Background()

	if operator != "" && shouldRecord {
		podIP := extractPodIP(respBody, target)
		if podIP != "" {
			log.Printf("[register] operator=%s → pod %s registrado no redis", operator, podIP)
			h.store.RecordArtifact(ctx, operator, podIP)
		}
	}

	// registra hit independente de shouldRecord
	if operator != "" {
		h.store.RecordArtifactHit(ctx, operator, target)
	}

	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := h.store.Ping(ctx); err != nil {
		http.Error(w, "redis unavailable", http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) DeletePod(w http.ResponseWriter, r *http.Request) {
	podIP := r.PathValue("ip")
	if podIP == "" {
		http.Error(w, "missing pod ip", http.StatusBadRequest)
		return
	}

	h.store.RemovePod(context.Background(), podIP)
	log.Printf("[delete] pod %s removido do redis e do cache local", podIP)
	w.WriteHeader(http.StatusNoContent)
}

// PodsHealth expõe o snapshot de saúde (memória/CPU) de todos os pods.
// GET /pods/health
func (h *Handler) PodsHealth(w http.ResponseWriter, r *http.Request) {
	infos := h.watcher.GetAllPodsInfo()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(infos)
}

// ArtifactStats expõe quantas vezes cada pod recebeu requisições para um artefato
// e o timestamp do último envio.
// GET /pods/stats?artifact={operator}
func (h *Handler) ArtifactStats(w http.ResponseWriter, r *http.Request) {
	operator := r.URL.Query().Get("artifact")
	if operator == "" {
		http.Error(w, "missing artifact param", http.StatusBadRequest)
		return
	}

	stats, err := h.store.GetArtifactStats(context.Background(), operator)
	if err != nil {
		http.Error(w, "redis error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func extractPodIP(body []byte, targetURL string) string {
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err == nil {
		if ip, ok := result["pod_ip"].(string); ok && ip != "" {
			return ip + ":5001"
		}
	}

	host := strings.TrimPrefix(targetURL, "http://")
	if host != "" && !strings.Contains(host, "svc.cluster.local") {
		return host
	}

	return ""
}
