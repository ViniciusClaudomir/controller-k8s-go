package store

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"controller/internal/cache"

	"github.com/redis/go-redis/v9"
)

type Store struct {
	rdb          *redis.Client
	cache        *cache.Cache
	spreadTarget float64
}

func New(c *cache.Cache) *Store {
	s := &Store{cache: c}

	s.rdb = redis.NewFailoverClient(&redis.FailoverOptions{
		MasterName:    "mymaster",
		SentinelAddrs: []string{"redis-sentinel.redis-ha.svc.cluster.local:26379"},
		Password:      "redis@HA2024",
	})

	s.spreadTarget = 0.3
	if v := os.Getenv("SPREAD_TARGET"); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil {
			s.spreadTarget = parsed
		}
	}

	return s
}

func (s *Store) SpreadTarget() float64 { return s.spreadTarget }

func (s *Store) GetPodsByArtifact(ctx context.Context, artifact string) ([]string, error) {
	if pods, ok := s.cache.Get(artifact); ok {
		return pods, nil
	}
	pods, err := s.rdb.SMembers(ctx, "artifact:"+artifact).Result()
	if err == nil && len(pods) > 0 {
		s.cache.Set(artifact, pods)
	}
	return pods, err
}

func (s *Store) GetTotalPods(ctx context.Context) int64 {
	total, _ := s.rdb.ZCard(ctx, "pods:all").Result()
	return total
}

func (s *Store) RecordArtifact(ctx context.Context, operator, podIP string) {
	s.rdb.SAdd(ctx, "artifact:"+operator, podIP)
	s.cache.Invalidate(operator)
}

func (s *Store) RemovePod(ctx context.Context, podIP string) {
	pipe := s.rdb.Pipeline()
	pipe.ZRem(ctx, "pods:all", podIP)

	iter := s.rdb.Scan(ctx, 0, "artifact:*", 100).Iterator()
	for iter.Next(ctx) {
		pipe.SRem(ctx, iter.Val(), podIP)
	}
	pipe.Exec(ctx)

	s.cache.InvalidateByPod(podIP)
}

func (s *Store) SyncPods(ctx context.Context, activeIPs []string) {
	now := float64(time.Now().Unix())

	if len(activeIPs) > 0 {
		members := make([]redis.Z, len(activeIPs))
		for i, ip := range activeIPs {
			members[i] = redis.Z{Score: now, Member: ip}
		}
		s.rdb.ZAdd(ctx, "pods:all", members...)
	}

	allInRedis, err := s.rdb.ZRange(ctx, "pods:all", 0, -1).Result()
	if err != nil {
		return
	}

	active := make(map[string]bool, len(activeIPs))
	for _, ip := range activeIPs {
		active[ip] = true
	}

	for _, ip := range allInRedis {
		if !active[ip] {
			s.RemovePod(ctx, fmt.Sprintf("%s", ip))
		}
	}
}

func (s *Store) Ping(ctx context.Context) error {
	return s.rdb.Ping(ctx).Err()
}

func (s *Store) SetRPS(ctx context.Context, podIP string, rps int64) {
	s.rdb.Set(ctx, "rps:"+podIP, rps, 30*time.Second)
}

func (s *Store) GetAllRPS(ctx context.Context) (map[string]float64, error) {
	keys, err := s.rdb.Keys(ctx, "rps:*").Result()
	if err != nil {
		return nil, err
	}

	result := make(map[string]float64, len(keys))
	for _, key := range keys {
		val, err := s.rdb.Get(ctx, key).Result()
		if err != nil {
			continue
		}
		rps, err := strconv.ParseFloat(val, 64)
		if err != nil {
			continue
		}
		result[strings.TrimPrefix(key, "rps:")] = rps
	}
	return result, nil
}

// — artifact hit stats —

type ArtifactStat struct {
	Count    int64     `json:"count"`
	LastSent time.Time `json:"last_sent"`
}

// RecordArtifactHit incrementa o contador de envios para (operator, podIP) e atualiza o timestamp.
func (s *Store) RecordArtifactHit(ctx context.Context, operator, podIP string) {
	key := fmt.Sprintf("artifact_stats:%s:%s", operator, podIP)
	pipe := s.rdb.Pipeline()
	pipe.HIncrBy(ctx, key, "count", 1)
	pipe.HSet(ctx, key, "last_sent", time.Now().UTC().Format(time.RFC3339))
	pipe.Exec(ctx)
}

// GetArtifactStats retorna contagem e último envio por pod para um determinado operator.
func (s *Store) GetArtifactStats(ctx context.Context, operator string) (map[string]ArtifactStat, error) {
	prefix := fmt.Sprintf("artifact_stats:%s:", operator)
	keys, err := s.rdb.Keys(ctx, prefix+"*").Result()
	if err != nil {
		return nil, err
	}

	result := make(map[string]ArtifactStat, len(keys))
	for _, key := range keys {
		vals, err := s.rdb.HGetAll(ctx, key).Result()
		if err != nil {
			continue
		}
		podIP := strings.TrimPrefix(key, prefix)
		count, _ := strconv.ParseInt(vals["count"], 10, 64)
		lastSent, _ := time.Parse(time.RFC3339, vals["last_sent"])
		result[podIP] = ArtifactStat{Count: count, LastSent: lastSent}
	}
	return result, nil
}
