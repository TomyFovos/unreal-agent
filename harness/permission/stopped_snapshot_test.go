package permission

import (
	"testing"
	"time"
)

func TestStoppedControllerDoesNotReportActiveFullAccess(t *testing.T) {
	c, ctx, key := humanController(t, Unrestricted(), time.Second)
	humanMode(t, c, key, FullAccess)
	c.Close()
	if c.Snapshot().Mode == FullAccess {
		t.Fatal("closed Controller still reports active Full Access")
	}
	p := getPermit(t, c.Request(ctx, Request{ID: "stopped", Tool: "write", Privileged: true}))
	if p.Denial == nil {
		t.Fatal("closed Controller granted an Operation")
	}
}
