package host

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
)

// Share a parent's stable template prefix without sharing conversation identity.
// Legacy children without template metadata retain their Session-specific key.
func (s *Session) modelCacheKey() string {
	var child struct{ ParentID, Template string }
	if json.Unmarshal(s.historyState.Configuration, &child) == nil && child.ParentID != "" && child.Template != "" {
		sum := sha256.Sum256([]byte("unreal-agent/child-cache/v1\x00" + child.ParentID + "\x00" + child.Template))
		return "child-cache-" + hex.EncodeToString(sum[:16])
	}
	return string(s.ID)
}
