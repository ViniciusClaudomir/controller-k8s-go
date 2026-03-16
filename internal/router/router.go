package router

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"

	"controller/internal/k8s"
	"controller/internal/store"
)

type Router struct {
	store   *store.Store
	watcher *k8s.Watcher
}

func New(s *store.Store, w *k8s.Watcher) *Router {
	return &Router{store: s, watcher: w}
}

// leastConnections retorna o pod com menos requisições em andamento.
// Em caso de empate, escolhe aleatoriamente entre os empatados.
func leastConnections(candidates []string, w *k8s.Watcher) string {
	best := candidates[0]
	bestCount := w.InFlightCount(best)
	for _, pod := range candidates[1:] {
		if count := w.InFlightCount(pod); count < bestCount {
			bestCount = count
			best = pod
		}
	}
	return best
}

func (r *Router) SubmitFallback() string {
	svc := os.Getenv("SUBMIT_SERVICE")
	if svc == "" {
		svc = "http://submit.submit.svc.cluster.local:5001"
	}
	return svc
}

// ChooseTarget retorna (targetURL, isCached, shouldRecord).
func (r *Router) ChooseTarget(ctx context.Context, operator string) (string, bool, bool) {
	if operator == "" {
		if pod := r.watcher.GetRandomPodIP(); pod != "" {
			log.Printf("[route][WARN] operator ausente → pod aleatório %s", pod)
			return fmt.Sprintf("http://%s", pod), false, false
		}
		fallback := r.SubmitFallback()
		log.Printf("[route][WARN] operator ausente e sem pods k8s → fallback service %s", fallback)
		return fallback, false, false
	}

	total := r.watcher.TotalPods()
	if total == 0 {
		log.Printf("[route][WARN] operator=%s watcher sem pods registrados", operator)
	}

	maxPods := int(math.Ceil(r.store.SpreadTarget() * float64(total)))
	if maxPods < 1 {
		maxPods = 1
	}

	pods, err := r.store.GetPodsByArtifact(ctx, operator)
	if err != nil {
		log.Printf("[route][ERROR] operator=%s erro ao buscar artefato no Redis: %v", operator, err)
	}

	// filtra pods do artefato que estão saudáveis
	var healthyArtifactPods []string
	for _, pod := range pods {
		if r.watcher.IsPodHealthy(pod) {
			healthyArtifactPods = append(healthyArtifactPods, pod)
		}
	}

	if len(pods) > 0 && len(healthyArtifactPods) < len(pods) {
		log.Printf("[route][WARN] operator=%s %d/%d pods do artefato filtrados por memória alta",
			operator, len(pods)-len(healthyArtifactPods), len(pods))
	}

	// sem cache ou todos os pods do artefato acima do threshold
	if err != nil || len(healthyArtifactPods) == 0 {
		if len(pods) > 0 && err == nil {
			log.Printf("[route][WARN] operator=%s todos os %d pods do artefato estão acima do threshold de memória → buscando pod saudável aleatório",
				operator, len(pods))
		}

		if pod := r.watcher.GetRandomPodIP(); pod != "" {
			shouldRecord := err != nil || len(pods) == 0
			log.Printf("[route][WARN] operator=%s sem pods saudáveis para o artefato → pod %s (shouldRecord=%v)", operator, pod, shouldRecord)
			return fmt.Sprintf("http://%s", pod), false, shouldRecord
		}

		// último recurso: qualquer pod, mesmo sobrecarregado
		if pod := r.watcher.GetRandomAnyPodIP(); pod != "" {
			log.Printf("[route][WARN] operator=%s TODOS os pods estão sobrecarregados → enviando para pod %s mesmo acima do threshold", operator, pod)
			return fmt.Sprintf("http://%s", pod), false, false
		}

		fallback := r.SubmitFallback()
		log.Printf("[route][ERROR] operator=%s sem pods disponíveis via k8s → fallback service %s", operator, fallback)
		return fallback, false, false
	}

	var ratio float64
	if total > 0 {
		ratio = float64(len(pods)) / float64(total)
	} else {
		ratio = 1.0
	}

	canSpread := len(pods) < maxPods
	randomRate := math.Max(0.1, r.store.SpreadTarget()-ratio)

	if rand.Float64() >= randomRate {
		chosen := leastConnections(healthyArtifactPods, r.watcher)
		log.Printf("[route] operator=%s cache hit → pod %s (ratio=%.2f healthy=%d maxPods=%d inflight=%d)",
			operator, chosen, ratio, len(healthyArtifactPods), maxPods, r.watcher.InFlightCount(chosen))
		return fmt.Sprintf("http://%s", chosen), true, false
	}

	if pod := r.watcher.GetRandomPodIP(); pod != "" {
		action := map[bool]string{true: "spreading", false: "escape 10%"}[canSpread]
		log.Printf("[route] operator=%s %s → pod %s (ratio=%.2f healthy=%d maxPods=%d)",
			operator, action, pod, ratio, len(healthyArtifactPods), maxPods)
		return fmt.Sprintf("http://%s", pod), false, canSpread
	}

	fallback := r.SubmitFallback()
	log.Printf("[route][ERROR] operator=%s sem pods saudáveis para spread → fallback service %s", operator, fallback)
	return fallback, false, false
}
