package mutation

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func checkFilesystem(file *os.File) error {
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &st); err != nil {
		return err
	}
	name := []byte{}
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	if string(name) != "apfs" && string(name) != "hfs" {
		return errors.New("unsupported mutation filesystem")
	}
	return nil
}
