//go:build !unix

package filesearch

import "os"

func openIgnore(root *os.Root, name string) (*os.File, error) { return root.Open(name) }
