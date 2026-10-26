//go:build linux || darwin

package mutation

import (
	"os"
	"syscall"
)

func footprintIdentity(info os.FileInfo) (FileIdentity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return FileIdentity{}, false
	}
	return FileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, true
}
