package mutation

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

// Explicit local filesystem support. Distributed/FUSE/9p semantics are not
// inferred from a successful rename; their crash/lock guarantees differ.
func checkFilesystem(file *os.File) error {
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &st); err != nil {
		return err
	}
	switch uint64(st.Type) {
	case 0xef53, 0x58465342, 0x9123683e, 0x01021994, 0x794c7630, 0x2fc12fc1, 0x858458f6:
		return nil
	default:
		return fmt.Errorf("unsupported mutation filesystem 0x%x", uint64(st.Type))
	}
}
