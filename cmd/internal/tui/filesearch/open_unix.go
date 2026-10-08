//go:build unix

package filesearch

import (
	"golang.org/x/sys/unix"
	"os"
)

func openIgnore(root *os.Root, name string) (*os.File, error) {
	// A concurrently substituted symlink/FIFO cannot escape or block the scan.
	return root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}
