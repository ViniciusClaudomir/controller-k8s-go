module controller

go 1.26.0

require (
	github.com/mwitkow/grpc-proxy v0.0.0-20230212185441-f345521cb9c9
	github.com/redis/go-redis/v9 v9.18.0
	golang.org/x/net v0.38.0
	google.golang.org/grpc v1.71.1
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.31.0 // indirect
	golang.org/x/text v0.23.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250115164207-1a7da9e5054f // indirect
	google.golang.org/protobuf v1.36.4 // indirect
)

// grpc-proxy depende do genproto monolítico antigo, mas grpc v1.71+ usa o split.
// Redirecionar para versão pós-split resolve o "ambiguous import".
replace google.golang.org/genproto => google.golang.org/genproto v0.0.0-20230822172742-b8732ec3820d
