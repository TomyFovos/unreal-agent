//go:build !linux && !darwin

package mutation

import "os"

func footprintIdentity(os.FileInfo) (FileIdentity, bool) { return FileIdentity{}, false }
