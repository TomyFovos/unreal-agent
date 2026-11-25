package subagent

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func (m *Manager) inspectChildren(ctx context.Context, p Plan) (string, error) {
	if m.usage == nil {
		return "No children in this Session.", nil
	}
	if p.Handle != "" {
		if _, err := m.ownedChild(p.Handle); err != nil {
			return "", err
		}
	}
	wait := time.Duration(min(int64(p.WaitSeconds), math.MaxInt64/int64(time.Second))) * time.Second
	if p.WaitSeconds == 0 {
		wait = 30 * time.Second
	}
	deadline := time.Now().Add(wait)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var watched map[operation.ID]bool
	for {
		report, err := m.usage.Read(ctx, p.ParentID, time.Now(), time.Hour)
		if err != nil {
			return "", err
		}
		if p.Action == "wait" && p.Handle == "" && watched == nil {
			watched = make(map[operation.ID]bool)
			for _, row := range report.TopChildren {
				if !terminalStatus(operation.Status(row.State)) {
					watched[row.Handle] = true
				}
			}
		}
		var lines []string
		var ready bool
		var ops map[operation.ID]operation.Operation
		if p.Action != "list" {
			view, err := m.config.Owner.Inspect(0, 1)
			if err != nil {
				return "", err
			}
			ops = make(map[operation.ID]operation.Operation, len(view.Operations))
			for _, op := range view.Operations {
				ops[op.ID] = op
			}
		}
		for _, row := range report.TopChildren {
			if p.Handle != "" && p.Handle != row.Handle ||
				p.Template != "" && p.Template != "*" && p.Template != row.Template ||
				len(watched) > 0 && !watched[row.Handle] {
				continue
			}
			ended := terminalStatus(operation.Status(row.State))
			if p.Action == "list" && !p.InspectAll && ended {
				continue
			}
			line := fmt.Sprintf("%s · %s · %s · %s", row.Handle, row.State, row.UsageText(), row.Title)
			if op, ok := ops[row.Handle]; ok {
				if handle, err := DecodeHandle(op); err == nil {
					if handle.Finish != nil {
						line += "\n" + handle.Finish.Result.Summary
					}
					for _, a := range handle.Approvals {
						line += fmt.Sprintf("\nApproval needed: %s %s %s", a.ID, a.Tool, a.Scope)
						ready = true
					}
				}
			}
			lines = append(lines, line)
			ready = ready || ended
		}
		if len(lines) == 0 {
			return "No matching children in this Session.", nil
		}
		if p.Action != "wait" || ready || !time.Now().Before(deadline) {
			return strings.Join(lines, "\n"), nil
		}
		select {
		case <-ctx.Done():
			return "", context.Cause(ctx)
		case <-ticker.C:
		}
	}
}
