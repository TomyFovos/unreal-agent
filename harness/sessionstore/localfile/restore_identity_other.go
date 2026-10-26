//go:build !linux

package localfile

import "os"

func restoreIdentityOf(os.FileInfo) (restoreIdentity, error) {
	return restoreIdentity{}, ErrRestoreViewUnsupported
}
