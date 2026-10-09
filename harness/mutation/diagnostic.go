package mutation

import (
	"errors"
	"os"
	"syscall"

	"github.com/unreallabsai/unreal-agent/harness/permission"
)

// Reason is a closed diagnostic, independent of permission decisions and of
// untrusted filenames/OS error text. Existing Code and durable CAS semantics
// remain authoritative; a reason never grants access or authorizes a retry.
type Reason string

const (
	ReasonInvalidRequest    Reason = "invalid_request"
	ReasonInvalidRevision   Reason = "invalid_revision"
	ReasonWorkspaceBoundary Reason = "workspace_boundary"
	ReasonInternalState     Reason = "internal_state_path"
	ReasonDuplicateTarget   Reason = "duplicate_target"
	ReasonParentMissing     Reason = "parent_missing"
	ReasonUnsafeParent      Reason = "unsafe_parent"
	ReasonInvalidTarget     Reason = "invalid_target"
	ReasonTargetTooLarge    Reason = "target_too_large"
	ReasonTargetUnavailable Reason = "target_unavailable"
	ReasonRevisionConflict  Reason = "revision_conflict"
	ReasonPermissionDenied  Reason = "permission_denied"
)

type diagnosticError struct{ reason Reason }

func (e diagnosticError) Error() string { return string(e.reason) }

func (r Reason) Valid() bool {
	switch r {
	case ReasonInvalidRequest, ReasonInvalidRevision, ReasonWorkspaceBoundary, ReasonInternalState, ReasonDuplicateTarget,
		ReasonParentMissing, ReasonUnsafeParent, ReasonInvalidTarget, ReasonTargetTooLarge, ReasonTargetUnavailable,
		ReasonRevisionConflict, ReasonPermissionDenied:
		return true
	}
	return false
}

// FailureReason never copies error text, paths, content or environment. In an
// absent-file Snapshot ENOENT is handled normally; ENOENT here means that the
// required parent could not be opened, not that exists:false is invalid.
func FailureReason(err error) Reason {
	if err == nil {
		return ""
	}
	var e diagnosticError
	if errors.As(err, &e) {
		return e.reason
	}
	if permission.Failure(err) != nil {
		return ReasonPermissionDenied
	}
	if os.IsNotExist(err) {
		return ReasonParentMissing
	}
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
		return ReasonUnsafeParent
	}
	return ReasonTargetUnavailable
}

func requestReason(r Request) Reason {
	for _, c := range r.Changes {
		if c.Expected.Validate() != nil {
			return ReasonInvalidRevision
		}
	}
	return ReasonInvalidRequest
}
