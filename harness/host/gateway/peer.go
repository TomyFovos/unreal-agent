package gateway

import (
	"context"
	"net"
)

type peerKey struct{}

func authenticatePeer(ctx context.Context, conn net.Conn) context.Context {
	return context.WithValue(ctx, peerKey{}, authorizedPeer(conn))
}
func authenticated(ctx context.Context) bool { ok, _ := ctx.Value(peerKey{}).(bool); return ok }
