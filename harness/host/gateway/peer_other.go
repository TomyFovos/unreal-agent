//go:build !linux && !darwin

package gateway

import "net"

func authorizedPeer(net.Conn) bool { return false }
