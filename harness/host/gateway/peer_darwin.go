//go:build darwin

package gateway

import (
	"golang.org/x/sys/unix"
	"net"
	"os"
)

func authorizedPeer(conn net.Conn) bool {
	u, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := u.SyscallConn()
	if err != nil {
		return false
	}
	var authorized bool
	err = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		authorized = e == nil && cred.Uid == uint32(os.Geteuid())
	})
	return err == nil && authorized
}
