package mutation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
)

// Recover reads outcome evidence for an already assigned durable operation ID.
// It never applies or replans a mutation. A present unfinished/corrupt receipt is
// indeterminate. Callers must not reuse an operation ID for a different action.
func (s *Service) Recover(ctx context.Context, id string) (Result, bool, error) {
	result := Result{Version: Version, Code: Indeterminate}
	if id == "" {
		return result, false, errors.New("operation identity is required")
	}
	lock, err := s.lock(ctx, "receipt:"+id)
	if err != nil {
		return result, false, err
	}
	defer lock.Close()
	key := sha256.Sum256([]byte(id))
	data, err := os.ReadFile(filepath.Join(s.state, hex.EncodeToString(key[:])+".receipt"))
	if errors.Is(err, os.ErrNotExist) {
		return Result{}, false, nil
	}
	if err != nil {
		return result, false, err
	}
	var saved receipt
	if json.Unmarshal(data, &saved) != nil || saved.Version != Version || saved.Result == nil || saved.Result.Version != Version {
		return result, true, nil
	}
	return *saved.Result, true, nil
}
