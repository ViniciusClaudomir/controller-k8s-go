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

// proxyClient com pool de conexões adequado para tráfego concorrente.
// http.DefaultClient usa MaxIdleConnsPerHost=2, insuficiente para 10+ threads.
var proxyClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true, // resposta já pode estar comprimida pelo upstream
	},
	Timeout: 60 * time.Second,
}

type Handler struct {
	router  *router.Router
	store   *store.Store
	metrics *metrics.Metrics
	watcher *k8s.Watcher
}

func New(r *router.Router, s *store.Store, m *metrics.Metrics, w *k8s.Watcher) *Handler {
	h := &Handler{router: r, store: s, metrics: m, watcher: w}
	// registra callback para pré-aquecer conexões TCP com novos pods
	w.SetWarmupFunc(h.warmupPods)
	return h
}

// warmupPods faz um HEAD /health em cada pod usando o proxyClient,
// forçando o pool de conexões a manter sockets abertos antes do tráfego real.
func (h *Handler) warmupPods(podIPs []string) {
	for _, ip := range podIPs {
		go func(ip string) {
			req, err := http.NewRequest(http.MethodHead, "http://"+ip+"/health", nil)
			if err != nil {
				return
			}
			resp, err := proxyClient.Do(req)
			if err != nil {
				log.Printf("[warmup] pod %s indisponível: %v", ip, err)
				return
			}
			resp.Body.Close()
			log.Printf("[warmup] conexão pré-aquecida com pod %s", ip)
		}(ip)
	}
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

	podIP := strings.TrimPrefix(target, "http://")
	h.watcher.IncrementInFlight(podIP)
	defer h.watcher.DecrementInFlight(podIP)

	targetURL := target + r.URL.Path

	// passa r.Body diretamente — sem io.ReadAll, sem buffer desnecessário
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		log.Printf("[submit][ERROR] operator=%s falha ao montar proxy request para %s: %v", operator, targetURL, err)
		http.Error(w, "proxy error", http.StatusBadGateway)
		return
	}
	proxyReq.Header = r.Header.Clone()
	proxyReq.ContentLength = r.ContentLength

	resp, err := proxyClient.Do(proxyReq)
	if err != nil {
		log.Printf("[submit][ERROR] operator=%s upstream %s não respondeu: %v", operator, targetURL, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		log.Printf("[submit][WARN] operator=%s upstream %s retornou %d", operator, targetURL, resp.StatusCode)
	}

	// copia headers de resposta para o cliente
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)

	// TeeReader: transmite a resposta ao cliente em streaming enquanto
	// captura o body para extração do pod IP (sem double-buffering).
	var bodyCopy bytes.Buffer
	if _, err := io.Copy(w, io.TeeReader(resp.Body, &bodyCopy)); err != nil {
		log.Printf("[submit][WARN] operator=%s stream interrompido para %s: %v", operator, targetURL, err)
	}

	// registros Redis de forma assíncrona — não bloqueia a resposta ao cliente
	if operator != "" {
		go func() {
			ctx := context.Background()
			if shouldRecord {
				podIP := extractPodIP(bodyCopy.Bytes(), target)
				if podIP != "" {
					log.Printf("[register] operator=%s → pod %s registrado no redis", operator, podIP)
					h.store.RecordArtifact(ctx, operator, podIP)
				} else {
					log.Printf("[register][WARN] operator=%s não foi possível extrair pod IP da resposta de %s", operator, target)
				}
			}
			h.store.RecordArtifactHit(ctx, operator, target)
		}()
	}
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
func (h *Handler) PodsHealth(w http.ResponseWriter, r *http.Request) {
	infos := h.watcher.GetAllPodsInfo()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(infos)
}

// ArtifactStats expõe contagem de hits e último envio por pod para um artefato.
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
