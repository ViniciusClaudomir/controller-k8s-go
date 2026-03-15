package k8s

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"controller/internal/store"
)

// PodInfo agrupa IP, nome e métricas de saúde de um pod.
type PodInfo struct {
	IP          string `json:"ip"`
	Name        string `json:"name"`
	MemoryUsage int64  `json:"memory_usage_bytes"`
	MemoryLimit int64  `json:"memory_limit_bytes"`
	CPUUsage    int64  `json:"cpu_usage_millicores"`
	CPULimit    int64  `json:"cpu_limit_millicores"`
}

func (p PodInfo) MemoryPct() float64 {
	if p.MemoryLimit <= 0 || p.MemoryUsage <= 0 {
		return 0
	}
	return float64(p.MemoryUsage) / float64(p.MemoryLimit) * 100
}

func (p PodInfo) CPUPct() float64 {
	if p.CPULimit <= 0 || p.CPUUsage <= 0 {
		return 0
	}
	return float64(p.CPUUsage) / float64(p.CPULimit) * 100
}

// — structs para deserializar a K8s API —

type k8sPodList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Resources struct {
					Limits struct {
						CPU    string `json:"cpu"`
						Memory string `json:"memory"`
					} `json:"limits"`
				} `json:"resources"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			PodIP      string `json:"podIP"`
			Phase      string `json:"phase"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

type k8sMetricsList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Containers []struct {
			Usage struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"usage"`
		} `json:"containers"`
	} `json:"items"`
}

// — Watcher —

type Watcher struct {
	store        *store.Store
	mu           sync.RWMutex
	pods         map[string]PodInfo // podIP → PodInfo
	client       *http.Client
	memThreshold float64
}

func NewWatcher(s *store.Store) *Watcher {
	caCert, _ := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caCert)

	threshold := 90.0
	if v := os.Getenv("MEM_THRESHOLD"); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil {
			threshold = parsed
		}
	}

	return &Watcher{
		store: s,
		pods:  make(map[string]PodInfo),
		client: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool},
			},
			Timeout: 5 * time.Second,
		},
		memThreshold: threshold,
	}
}

func (w *Watcher) Start() {
	w.refresh()
	go func() {
		for {
			time.Sleep(10 * time.Second)
			w.refresh()
		}
	}()
}

// GetRandomPodIP retorna um pod saudável aleatório (abaixo do threshold de memória).
func (w *Watcher) GetRandomPodIP() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	healthy := w.healthyIPs()
	if len(healthy) == 0 {
		return ""
	}
	return healthy[rand.Intn(len(healthy))]
}

// GetRandomAnyPodIP retorna qualquer pod (incluindo sobrecarregados) como último recurso.
func (w *Watcher) GetRandomAnyPodIP() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	all := make([]string, 0, len(w.pods))
	for ip := range w.pods {
		all = append(all, ip)
	}
	if len(all) == 0 {
		return ""
	}
	return all[rand.Intn(len(all))]
}

// IsPodHealthy retorna true se o pod estiver abaixo do threshold de memória.
func (w *Watcher) IsPodHealthy(podIP string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	info, ok := w.pods[podIP]
	if !ok {
		return true // pod desconhecido: assume saudável
	}
	return info.MemoryPct() < w.memThreshold
}

// GetAllPodsInfo retorna snapshot das infos de todos os pods para o endpoint de saúde.
func (w *Watcher) GetAllPodsInfo() []PodInfo {
	w.mu.RLock()
	defer w.mu.RUnlock()
	result := make([]PodInfo, 0, len(w.pods))
	for _, info := range w.pods {
		result = append(result, info)
	}
	return result
}

// healthyIPs deve ser chamado com RLock já adquirido.
func (w *Watcher) healthyIPs() []string {
	healthy := make([]string, 0, len(w.pods))
	for ip, info := range w.pods {
		if info.MemoryPct() < w.memThreshold {
			healthy = append(healthy, ip)
		}
	}
	return healthy
}

// — refresh —

func (w *Watcher) refresh() {
	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return
	}
	tokenStr := string(token)

	namespace := os.Getenv("SUBMIT_NAMESPACE")
	if namespace == "" {
		namespace = "submit"
	}
	label := os.Getenv("SUBMIT_LABEL")
	if label == "" {
		label = "app=submit"
	}
	port := os.Getenv("SUBMIT_PORT")
	if port == "" {
		port = "5001"
	}

	// 1. lista de pods com spec (limites de CPU/memória)
	podInfos, err := w.fetchPodList(tokenStr, namespace, label, port)
	if err != nil {
		log.Printf("[k8s] erro ao buscar pods: %v", err)
		return
	}

	// 2. métricas de uso atual (best-effort — metrics-server pode estar ausente)
	metricsMap, err := w.fetchMetrics(tokenStr, namespace, label)
	if err != nil {
		log.Printf("[k8s] aviso: metrics-server indisponível: %v", err)
	}

	// 3. merge: injeta uso atual nos pods
	newPods := make(map[string]PodInfo, len(podInfos))
	ips := make([]string, 0, len(podInfos))
	for _, p := range podInfos {
		if m, ok := metricsMap[p.Name]; ok {
			p.MemoryUsage = m.memUsage
			p.CPUUsage = m.cpuUsage
		}
		newPods[p.IP] = p
		ips = append(ips, p.IP)
	}

	w.mu.Lock()
	w.pods = newPods
	w.mu.Unlock()

	log.Printf("[k8s] pods ativos: %v", ips)
	w.store.SyncPods(context.Background(), ips)
}

type podUsage struct{ memUsage, cpuUsage int64 }

func (w *Watcher) fetchPodList(token, namespace, label, port string) ([]PodInfo, error) {
	url := fmt.Sprintf(
		"https://kubernetes.default.svc/api/v1/namespaces/%s/pods?labelSelector=%s",
		namespace, label,
	)
	body, err := w.k8sGet(url, token)
	if err != nil {
		return nil, err
	}

	var pl k8sPodList
	if err := json.Unmarshal(body, &pl); err != nil {
		return nil, err
	}

	var result []PodInfo
	for _, pod := range pl.Items {
		if pod.Status.Phase != "Running" {
			continue
		}
		ready := false
		for _, c := range pod.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready = true
				break
			}
		}
		if !ready {
			continue
		}

		var memLimit, cpuLimit int64
		for _, c := range pod.Spec.Containers {
			memLimit += parseMemory(c.Resources.Limits.Memory)
			cpuLimit += parseCPU(c.Resources.Limits.CPU)
		}

		result = append(result, PodInfo{
			IP:          fmt.Sprintf("%s:%s", pod.Status.PodIP, port),
			Name:        pod.Metadata.Name,
			MemoryLimit: memLimit,
			CPULimit:    cpuLimit,
		})
	}
	return result, nil
}

func (w *Watcher) fetchMetrics(token, namespace, label string) (map[string]podUsage, error) {
	url := fmt.Sprintf(
		"https://kubernetes.default.svc/apis/metrics.k8s.io/v1beta1/namespaces/%s/pods?labelSelector=%s",
		namespace, label,
	)
	body, err := w.k8sGet(url, token)
	if err != nil {
		return nil, err
	}

	var ml k8sMetricsList
	if err := json.Unmarshal(body, &ml); err != nil {
		return nil, err
	}

	result := make(map[string]podUsage, len(ml.Items))
	for _, item := range ml.Items {
		var mem, cpu int64
		for _, c := range item.Containers {
			mem += parseMemory(c.Usage.Memory)
			cpu += parseCPU(c.Usage.CPU)
		}
		result[item.Metadata.Name] = podUsage{memUsage: mem, cpuUsage: cpu}
	}
	return result, nil
}

func (w *Watcher) k8sGet(url, token string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// — parsers de quantidades Kubernetes —

func parseMemory(s string) int64 {
	if s == "" {
		return 0
	}
	units := []struct {
		suffix string
		factor int64
	}{
		{"Ki", 1024},
		{"Mi", 1024 * 1024},
		{"Gi", 1024 * 1024 * 1024},
		{"Ti", 1024 * 1024 * 1024 * 1024},
		{"K", 1000},
		{"M", 1000 * 1000},
		{"G", 1000 * 1000 * 1000},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			n, _ := strconv.ParseInt(strings.TrimSuffix(s, u.suffix), 10, 64)
			return n * u.factor
		}
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func parseCPU(s string) int64 {
	if s == "" {
		return 0
	}
	if strings.HasSuffix(s, "m") {
		n, _ := strconv.ParseInt(strings.TrimSuffix(s, "m"), 10, 64)
		return n // millicores
	}
	if strings.HasSuffix(s, "n") {
		n, _ := strconv.ParseInt(strings.TrimSuffix(s, "n"), 10, 64)
		return n / 1_000_000 // nanocores → millicores
	}
	f, _ := strconv.ParseFloat(s, 64)
	return int64(f * 1000) // cores → millicores
}
