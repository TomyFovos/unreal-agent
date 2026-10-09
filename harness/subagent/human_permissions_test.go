package subagent_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
)

func TestHumanChildPermissionSubsetAndNoDelegatedApproval(t *testing.T) {
	for _, tc := range []struct {
		name              string
		mode              permission.Mode
		writable, allowed bool
	}{
		{"once-readonly", permission.Ask, false, true},
		{"once-write-denied", permission.Ask, true, false},
		{"full-bounded-child", permission.FullAccess, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			policy := permission.Config{Tools: []string{"Finish", "SendParent"}, ReadRoots: []string{dir}}
			if tc.writable {
				policy.WriteRoots = []string{dir}
			}
			templates := map[string]sub.Template{"worker": {Workspace: dir, Runtime: jsontext.Value(`{"provider":"fake"}`), Policy: policy}}
			var calls atomic.Int32
			model := adapter(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
				if calls.Add(1) == 1 {
					return response("SubagentStart", `{"template":"worker","task":"finish"}`), nil
				}
				return llm.Response{}, nil
			})
			h, err := host.New(t.Context(), host.Config{Directory: dir, PermissionMaximum: permission.Unrestricted(), Build: factory(sub.Config{Directory: dir, Binary: binary, Arguments: []string{"-test.run=^TestChildServerHelper$"}, Environment: append(os.Environ(), "UNREAL_SUBAGENT_HELPER=1"), Templates: templates}, model)})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			s, err := h.Create(t.Context(), host.Options{ID: "parent", Policy: permission.Unrestricted()})
			if err != nil {
				t.Fatal(err)
			}
			c, err := s.PermissionControl(s.Generation)
			if err != nil {
				t.Fatal(err)
			}
			key, release := c.AcquireControl()
			defer release()
			rev := c.Snapshot().Revision
			proof := ""
			if tc.mode == permission.FullAccess {
				proof, err = c.PrepareFull(key, rev)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err = c.SetMode(key, rev, tc.mode, proof); err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal("start fake child")
			if _, err = s.Submit(t.Context(), s.Generation, inbox.Input{ID: "task", Kind: inbox.InputExternal, Payload: payload}); err != nil {
				t.Fatal(err)
			}
			if tc.mode == permission.Ask {
				deadline := time.Now().Add(5 * time.Second)
				for {
					v, changed := c.Watch()
					if len(v.Pending) == 1 {
						if err = c.Decide(key, v.Revision, v.Pending[0].ID, true); err != nil {
							t.Fatal(err)
						}
						break
					}
					select {
					case <-changed:
					case <-time.After(time.Until(deadline)):
						t.Fatal("child approval not requested")
					}
				}
			}
			result := awaitChild(t, s, true)
			if result.Permission == nil {
				t.Fatal("child start missing canonical permission")
			}
			if tc.allowed {
				if result.Status != operation.StatusCompleted {
					t.Fatal(result)
				}
			} else {
				if result.Status != operation.StatusFailed || result.Denial == nil {
					t.Fatal("writable child received parent one-time grant", result)
				}
				p, _ := sub.DecodePlan(result)
				if _, err = os.Stat(filepath.Join(dir, string(p.ChildID)+".session.jsonl")); !os.IsNotExist(err) {
					t.Fatal("unauthorized child spawned")
				}
			}
			view, _ := s.Inspect(0, 256)
			raw, _ := json.Marshal(view.History)
			if strings.Contains(string(raw), key) || strings.Contains(string(raw), proof) && proof != "" {
				t.Fatal("child inherited private human approval")
			}
		})
	}
}
