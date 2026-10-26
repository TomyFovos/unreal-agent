package localfile

import (
	"os"
	"syscall"
)

func restoreIdentityOf(info os.FileInfo) (restoreIdentity, error) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return restoreIdentity{}, ErrRestoreViewUnsupported
	}
	return restoreIdentity{device: uint64(s.Dev), inode: s.Ino, size: s.Size, modifiedSeconds: s.Mtim.Sec, modifiedNanos: s.Mtim.Nsec, changedSeconds: s.Ctim.Sec, changedNanos: s.Ctim.Nsec}, nil
}
