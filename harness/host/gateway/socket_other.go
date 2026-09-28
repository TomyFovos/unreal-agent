//go:build !linux && !darwin

package gateway

import (
	"errors"
	"net"
)

func listen(string) (net.Listener, func(), error) {
	return nil, nil, errors.New("gateway: local sockets require Linux/macOS; use WSL on Windows")
}
