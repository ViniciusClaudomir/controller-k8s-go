# artifact-registry (controller)

Proxy/roteador inteligente para o namespace `submit`. Recebe requisições HTTP e gRPC, decide qual pod do `submit` deve processá-las com base em afinidade de artefato, saúde dos pods e estratégia de spread, e registra métricas de uso no Redis.

---

## Visão geral da arquitetura

```
Cliente (HTTP/gRPC)
        │
        ▼
┌─────────────────────────────────┐
│         artifact-registry       │  ← este serviço (namespace: artifact-registry)
│                                 │
│  ┌──────────┐  ┌─────────────┐  │
│  │  handler │  │ grpc-proxy  │  │
│  └────┬─────┘  └──────┬──────┘  │
│       │               │         │
│       └──────┬─────────┘        │
│              ▼                  │
│         ┌────────┐              │
│         │ router │              │
│         └───┬────┘              │
│    ┌─────────┼──────────┐       │
│    ▼         ▼          ▼       │
│  store    watcher    metrics    │
│ (Redis)   (K8s API)  (req/seg)  │
└─────────────────────────────────┘
        │              │
        ▼              ▼
   Redis HA       Pods submit
  (Sentinel)   (namespace: submit)
```

---

## Como funciona

### 1. Descoberta de pods (`k8s`)

A cada **10 segundos** o watcher consulta a K8s API em duas chamadas:

| Chamada | Endpoint | Para que serve |
|---------|----------|----------------|
| Pod list | `/api/v1/namespaces/submit/pods?labelSelector=app=submit` | IPs, nomes, limites de CPU/memória do spec |
| Metrics API | `/apis/metrics.k8s.io/v1beta1/namespaces/submit/pods` | Uso atual de CPU e memória |

Os dois resultados são mesclados por nome de pod, gerando um `PodInfo` com:
- `ip` — endereço `IP:porta` do pod
- `memory_usage_bytes` / `memory_limit_bytes`
- `cpu_usage_millicores` / `cpu_limit_millicores`

Depois de montar o mapa, o watcher sincroniza o Redis (`pods:all`) removendo pods que sumiram do namespace.

### 2. Filtro de saúde

Pods com **memória ≥ `MEM_THRESHOLD`%** (padrão `90%`) são excluídos da seleção normal.

Ordem de fallback quando não há pod saudável com o artefato:

```
pods saudáveis com artefato
        │  nenhum saudável
        ▼
qualquer pod saudável (sem o artefato)
        │  todos sobrecarregados
        ▼
qualquer pod (mesmo acima do threshold)
        │  nenhum pod via K8s
        ▼
   fallback service (SUBMIT_SERVICE)
```

### 3. Roteamento por artefato (`router`)

O `operator` chega no header HTTP ou no metadata gRPC. O router decide o destino assim:

```
1. Busca pods que já processaram esse operator no Redis (cache local de 5s)
2. Filtra pelos saudáveis
3. Calcula ratio = pods_com_artefato / total_pods
4. Calcula randomRate = max(0.1, SPREAD_TARGET - ratio)
5. Se rand() >= randomRate → usa pod do cache (hit)
6. Se rand() <  randomRate → usa pod aleatório saudável (spread)
```

O **spread** garante que novos pods recebam o artefato gradualmente até atingir `SPREAD_TARGET` (padrão `25%` dos pods).

### 4. Cache de afinidade (`store` + `cache`)

```
Redis                          Cache local (in-memory, TTL 5s)
─────────────────────          ──────────────────────────────
artifact:{operator}  ←──────→  operator → []podIPs
  (Set de podIPs)
pods:all
  (ZSet score=unix_ts)
artifact_stats:{op}:{ip}
  (Hash: count, last_sent)
rps:{ip}
  (String, TTL 30s)
```

Quando um pod é **adicionado** ao artefato (`RecordArtifact`), o cache local é invalidado para garantir consistência. Quando um pod é **removido** (`RemovePod`), o cache é varrido e todas as chaves que contêm aquele IP são removidas.

### 5. Métricas de req/seg (`metrics`)

A cada segundo, para cada pod que recebeu requisições, o contador atômico local é drenado e o valor é escrito no Redis como `rps:{ip}` com TTL de 30 segundos.

### 6. Estatísticas de artefato

A cada requisição roteada com `operator` definido, é feito um `HINCRBY` no Redis:

```
artifact_stats:{operator}:{podIP}
  count     → número total de envios
  last_sent → timestamp UTC do último envio (RFC3339)
```

---

## Endpoints HTTP

| Método | Path | Descrição |
|--------|------|-----------|
| `ANY` | `/submit{path}` | Proxy para o pod escolhido pelo router |
| `GET` | `/lookup?artifact={op}` | Retorna um dos pods que têm o artefato |
| `GET` | `/health` | Liveness/readiness — verifica conexão com Redis |
| `GET` | `/pods/health` | Snapshot de memória e CPU de todos os pods |
| `GET` | `/pods/metrics` | req/seg por pod (lido do Redis) |
| `GET` | `/pods/stats?artifact={op}` | Contagem de hits e último envio por pod para um artefato |
| `POST` | `/delete/{ip}` | Remove um pod do Redis e invalida o cache local |

### gRPC

Qualquer chamada gRPC com `Content-Type: application/grpc` é transparentemente proxiada usando `grpc-proxy`. O `operator` é lido do metadata gRPC.

---

## Variáveis de ambiente

| Variável | Padrão | Descrição |
|----------|--------|-----------|
| `SPREAD_TARGET` | `0.3` | Fração de pods que devem ter o artefato (ex: `0.25` = 25%) |
| `MEM_THRESHOLD` | `90` | % de uso de memória acima do qual o pod é ignorado |
| `SUBMIT_NAMESPACE` | `submit` | Namespace dos pods alvo |
| `SUBMIT_LABEL` | `app=submit` | Label selector para filtrar pods |
| `SUBMIT_PORT` | `5001` | Porta dos pods submit |
| `SUBMIT_SERVICE` | `http://submit.submit.svc.cluster.local:5001` | Fallback quando não há pods disponíveis |

---

## Estrutura do código

```
controller/
├── main.go                      # wiring: instancia e conecta todos os pacotes
└── internal/
    ├── cache/
    │   └── cache.go             # cache in-memory com TTL, get/set/invalidate
    ├── store/
    │   └── store.go             # todas as operações Redis
    ├── k8s/
    │   └── k8s.go               # watcher: descobre pods, coleta métricas de saúde
    ├── router/
    │   └── router.go            # lógica de roteamento e spread
    ├── metrics/
    │   └── metrics.go           # contadores de req/seg por pod
    ├── grpcproxy/
    │   └── grpc.go              # proxy transparente gRPC
    └── handler/
        └── handler.go           # handlers HTTP
```

### Dependências do grafo de pacotes (sem ciclos)

```
cache ← store ← k8s
                  ↓
              router ← grpcproxy
                  ↓
              handler ← metrics ← store
```

---

## Dependências Go

| Pacote | Versão | Para que serve |
|--------|--------|----------------|
| `github.com/redis/go-redis/v9` | v9.18.0 | Cliente Redis com suporte a Sentinel |
| `github.com/mwitkow/grpc-proxy` | v0.0.0-20230212 | Proxy gRPC transparente (sem deserializar payload) |
| `google.golang.org/grpc` | v1.71.1 | Servidor e cliente gRPC |
| `golang.org/x/net` | v0.38.0 | HTTP/2 com h2c (plaintext) |

> **Nota sobre genproto:** `grpc-proxy` depende do `genproto` monolítico antigo. O `go.mod` usa `replace` para apontá-lo para a versão pós-split e evitar `ambiguous import`.

---

## Infraestrutura Kubernetes (https://github.com/ViniciusClaudomir/k8s)

### artifact-registry (`k8s/artifact-registry/`)

| Arquivo | Recurso | Detalhe |
|---------|---------|---------|
| `01-namespace.yaml` | `Namespace` | `artifact-registry` |
| `02-deployment.yaml` | `Deployment` | 2 réplicas, imagem `artifact-registry:v11`, `SPREAD_TARGET=0.25` |
| `03-service.yaml` | `Service` (ClusterIP) | porta `8080` |
| `04-hpa.yaml` | `HorizontalPodAutoscaler` | 1–10 réplicas, escala em CPU 70% ou memória 80% |
| `05-destinationrule.yaml` | `DestinationRule` (Istio) | circuit breaker: ejeção após 5 erros 5xx consecutivos, máx 50% pods ejetados |
| `06-rbac.yaml` | `ServiceAccount` + `Role` + `RoleBinding` | permissão de `list`/`watch` em pods no namespace `submit` |

### Redis HA (`k8s/redis/`)

Topologia: **1 master + 1 replica + 2 sentinels**, todos no namespace `redis-ha`.

| Componente | Tipo | Réplicas | Storage |
|------------|------|----------|---------|
| `redis-master` | StatefulSet | 1 | 1 Gi (RWO) |
| `redis-replica` | StatefulSet | 1 | — |
| `redis-sentinel` | StatefulSet | 2 | — |

Configuração do Sentinel:
- Master: `redis-master-0.redis-master.redis-ha.svc.cluster.local:6379`
- Quorum: **2** (ambos os sentinels precisam concordar para failover)
- `down-after-milliseconds`: 300 ms
- Senha: `redis@HA2024`

O cliente Go se conecta via `redis-sentinel.redis-ha.svc.cluster.local:26379`.

---

## Deploy

```bash
# 1. Redis HA
kubectl apply -f k8s/redis/00-namespace.yaml
kubectl apply -f k8s/redis/master/
kubectl apply -f k8s/redis/replica/
kubectl apply -f k8s/redis/sentinel/

# 2. artifact-registry
kubectl apply -f k8s/artifact-registry/

# 3. Build da imagem (exemplo com minikube)
eval $(minikube docker-env)
docker build -t artifact-registry:v11 ./controller
```

---

## Fluxo completo de uma requisição

```
1. Cliente envia POST /submit com header "operator: bert"
2. handler.Submit → router.ChooseTarget(ctx, "bert")
3. router busca pods com artefato "bert" no Redis (via cache local)
4. Filtra pelos saudáveis (memória < MEM_THRESHOLD)
5. Decide: cache hit (prob alta) ou spread (prob = randomRate)
6. Retorna target: "http://10.244.1.5:5001"
7. handler faz proxy da requisição para o pod escolhido
8. Se shouldRecord=true: registra "bert" → "10.244.1.5:5001" no Redis
9. Registra hit: artifact_stats:bert:10.244.1.5:5001 {count++, last_sent}
10. metrics.IncrementPodRequest("10.244.1.5:5001")
11. A cada 1s: drena contadores e escreve rps:{ip} no Redis
```
