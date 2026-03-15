package grpcproxy

import (
	"context"
	"log"
	"strings"

	proxy "github.com/mwitkow/grpc-proxy/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"

	"controller/internal/router"
	"controller/internal/store"
)

type codecWrapper struct{ grpc.Codec }

func (c codecWrapper) Name() string { return c.String() }

var _ encoding.Codec = codecWrapper{}

type Director struct {
	router *router.Router
	store  *store.Store
}

func NewDirector(r *router.Router, s *store.Store) *Director {
	return &Director{router: r, store: s}
}

func (d *Director) Direct(ctx context.Context, fullMethod string) (context.Context, *grpc.ClientConn, error) {
	md, _ := metadata.FromIncomingContext(ctx)

	operator := ""
	if vals := md.Get("operator"); len(vals) > 0 {
		operator = vals[0]
	}

	target, _, shouldRecord := d.router.ChooseTarget(ctx, operator)
	podAddr := strings.TrimPrefix(target, "http://")

	if operator != "" && shouldRecord && !strings.Contains(podAddr, "svc.cluster.local") {
		log.Printf("[register][grpc] operator=%s → pod %s registrado no redis", operator, podAddr)
		d.store.RecordArtifact(ctx, operator, podAddr)
	}

	conn, err := grpc.NewClient(
		podAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(codecWrapper{proxy.Codec()})),
	)
	if err != nil {
		return ctx, nil, err
	}

	outCtx := metadata.NewOutgoingContext(ctx, md.Copy())
	return outCtx, conn, nil
}
