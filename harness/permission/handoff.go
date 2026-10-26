package permission

import "context"

// WithCurrentRevision serializes an already-authorized control handoff with
// mode changes and revocation. It grants no permission. The caller must first
// authorize the exact operation resources using its scoped policy.
//
// send must only hand off a pre-encoded bounded control message. It must have
// an I/O deadline, must not await execution or a reply, and must not re-enter
// this Controller. Mode changes accepted before this handoff prevent send;
// changes after send starts concern an in-flight operation and cannot imply
// rollback. Ordinary local execution does not use this optional boundary.
func (c *Controller) WithCurrentRevision(ctx context.Context, revision uint64, send func() error) error {
	if c == nil || ctx == nil || send == nil {
		return deny("approval", "authorization handoff unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() != nil || c.ctx.Err() != nil || c.stopped || revision == 0 || c.revision != revision {
		return deny("approval", "authorization handoff revoked")
	}
	return send()
}
