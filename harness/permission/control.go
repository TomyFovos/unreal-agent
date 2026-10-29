package permission

import "context"

// ControlRequest/Reply are private local control messages. The lease is
// transient authority; never serialize it as canonical items or context.
type ControlRequest struct {
	Action    string
	Lease     string `json:",omitzero"`
	Expected  uint64
	Mode      Mode   `json:",omitzero"`
	Operation string `json:",omitzero"`
	Allow     bool
}
type ControlReply struct {
	Snapshot ModeSnapshot
	Lease    string `json:",omitzero"`
}

type ControlConnection struct {
	Initial ModeSnapshot
	Updates <-chan ModeSnapshot
	Cancel  func()
	Request func(context.Context, ControlRequest) (ControlReply, error)
}
