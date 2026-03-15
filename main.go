package main

import (
	"net/http"
	"strings"
	"time"

	proxy "github.com/mwitkow/grpc-proxy/proxy"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"

	"controller/internal/cache"
	"controller/internal/grpcproxy"
	"controller/internal/handler"
	"controller/internal/k8s"
	"controller/internal/metrics"
	"controller/internal/router"
	"controller/internal/store"
)

func main() {
	c := cache.New(5 * time.Second)
	s := store.New(c)

	w := k8s.NewWatcher(s)
	w.Start()

	r := router.New(s, w)

	m := metrics.New(s)
	m.StartLoop()

	dir := grpcproxy.NewDirector(r, s)
	h := handler.New(r, s, m, w)

	grpcServer := grpc.NewServer(
		grpc.UnknownServiceHandler(proxy.TransparentHandler(dir.Direct)),
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/lookup", h.Lookup)
	mux.HandleFunc("/submit", h.Submit)
	mux.HandleFunc("/health", h.Health)
	mux.HandleFunc("/pods/metrics", m.HandlePodMetrics)
	mux.HandleFunc("/pods/health", h.PodsHealth)
	mux.HandleFunc("/pods/stats", h.ArtifactStats)
	mux.HandleFunc("POST /delete/{ip}", h.DeletePod)

	mixed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
		} else {
			mux.ServeHTTP(w, r)
		}
	})

	http.ListenAndServe(":8080", h2c.NewHandler(mixed, &http2.Server{}))
}
