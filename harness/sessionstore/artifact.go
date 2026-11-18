package sessionstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

const HostArtifact = "artifact"

// ArtifactRecord binds an explicitly captured file version to the original
// path and a Session-owned attachment. A fork keeps the source attachment owner.
type ArtifactRecord struct {
	Owner  session.ID
	Path   string
	SHA256 string
	Size   int64
}

func (a ArtifactRecord) Validate() error {
	digest, err := hex.DecodeString(a.SHA256)
	if a.Owner == "" || !filepath.IsAbs(a.Path) || err != nil || len(digest) != sha256.Size ||
		strings.ToLower(a.SHA256) != a.SHA256 || a.Size < 0 {
		return fmt.Errorf("invalid artifact identity")
	}
	return nil
}

func (a ArtifactRecord) Locator(id session.ID, seq Sequence) record.Locator {
	return record.Address{Session: string(id), Sequence: uint64(seq), Kind: record.Artifact, Path: a.Path, Hash: a.SHA256}.Locator()
}
