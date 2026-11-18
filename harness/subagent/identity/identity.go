// Package identity defines canonical parent/operation-derived child identities.
package identity

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

func ChildID(parent session.ID, id operation.ID) session.ID {
	sum := sha256.Sum256([]byte("unreal-agent/subagent/v1\x00" + string(parent) + "\x00" + string(id)))
	return session.ID("child-" + hex.EncodeToString(sum[:16]))
}
