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

func (r *Router) SubmitFallback() string {
	svc := os.Getenv("SUBMIT_SERVICE")
	if svc == "" {
		svc = "http://submit.submit.svc.cluster.local:5001"
	}
	return svc
}

// ChooseTarget retorna (targetURL, isCached, shouldRecord).
//
// Regras de saúde:
//   - Pods com memória >= MEM_THRESHOLD são excluídos da seleção normal.
//   - Se todos os pods do artefato estiverem acima do threshold, roteia para
//     qualquer pod saudável (sem artefato).
//   - Último recurso: qualquer pod (mesmo sobrecarregado) ou fallback service.
func (r *Router) ChooseTarget(ctx context.Context, operator string) (string, bool, bool) {
	if operator == "" {
		log.Printf("[route] operator vazio → pod saudável aleatório")
		if pod := r.watcher.GetRandomPodIP(); pod != "" {
			return fmt.Sprintf("http://%s", pod), false, false
		}
		return r.SubmitFallback(), false, false
	}

	total := r.store.GetTotalPods(ctx)
	maxPods := int(math.Ceil(r.store.SpreadTarget() * float64(total)))
	if maxPods < 1 {
		maxPods = 1
	}

	pods, err := r.store.GetPodsByArtifact(ctx, operator)

	// filtra pods do artefato que estão saudáveis
	var healthyArtifactPods []string
	for _, pod := range pods {
		if r.watcher.IsPodHealthy(pod) {
			healthyArtifactPods = append(healthyArtifactPods, pod)
		}
	}

	// sem cache ou todos os pods do artefato acima do threshold
	if err != nil || len(healthyArtifactPods) == 0 {
		if len(pods) > 0 && err == nil {
			log.Printf("[route] operator=%s todos os %d pods do artefato acima do threshold de memória → buscando pod saudável", operator, len(pods))
		}

		// tenta qualquer pod saudável (sem artefato)
		if pod := r.watcher.GetRandomPodIP(); pod != "" {
			shouldRecord := err != nil || len(pods) == 0 // só registra artefato se era "sem cache"
			log.Printf("[route] operator=%s → pod saudável %s (shouldRecord=%v)", operator, pod, shouldRecord)
			return fmt.Sprintf("http://%s", pod), false, shouldRecord
		}

		// último recurso: qualquer pod, mesmo sobrecarregado
		if pod := r.watcher.GetRandomAnyPodIP(); pod != "" {
			log.Printf("[route] operator=%s → último recurso (todos sobrecarregados) pod %s", operator, pod)
			return fmt.Sprintf("http://%s", pod), false, false
		}

		return r.SubmitFallback(), false, false
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
		chosen := healthyArtifactPods[rand.Intn(len(healthyArtifactPods))]
		log.Printf("[route] operator=%s cache hit → pod %s (ratio=%.2f healthy=%d maxPods=%d)",
			operator, chosen, ratio, len(healthyArtifactPods), maxPods)
		return fmt.Sprintf("http://%s", chosen), true, false
	}

	if pod := r.watcher.GetRandomPodIP(); pod != "" {
		log.Printf("[route] operator=%s %s → pod %s (ratio=%.2f healthy=%d maxPods=%d)",
			operator, map[bool]string{true: "spreading", false: "escape 10%"}[canSpread],
			pod, ratio, len(healthyArtifactPods), maxPods)
		return fmt.Sprintf("http://%s", pod), false, canSpread
	}

	return r.SubmitFallback(), false, false
}
