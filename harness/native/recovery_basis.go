package native

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

type executionBasis struct {
	Version    int                `json:"version"`
	Action     string             `json:"action"`
	PlanSHA256 string             `json:"plan_sha256"`
	Footprint  mutation.Footprint `json:"footprint"`
}

func recoveryRequest(current operation.Operation) (Request, operation.RemoteJobState, error) {
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil || state.Plan.Type != PlanType || state.Plan.Version != Version {
		return Request{}, state, operation.ErrIndependenceUnproven
	}
	var r Request
	if json.Unmarshal(state.Plan.Data, &r, json.RejectUnknownMembers(true)) != nil || r.Validate() != nil || current.ToolName != "" && current.ToolName != r.Action {
		return r, state, operation.ErrIndependenceUnproven
	}
	return r, state, nil
}

// registeredPreparation belongs only to the authorized handler worker. It is
// never published, persisted, cached, or accepted as external authority.
type registeredPreparation struct {
	plan recoveryPlan
	err  error
}

// Registration has already validated and detached this exact Operation state.
// Preserve Add's established synchronous weak-parse errors; strict validation
// failures remain asynchronous, before preparation or any executor effect.
func prepareRegisteredPlan(current operation.Operation, state operation.RemoteJobState) (Request, *registeredPreparation, error) {
	var request Request
	if err := json.Unmarshal(state.Plan.Data, &request, json.RejectUnknownMembers(true)); err != nil {
		request = Request{}
		if err := json.Unmarshal(state.Plan.Data, &request); err != nil {
			return request, nil, err
		}
		return request, &registeredPreparation{err: operation.ErrIndependenceUnproven}, nil
	}
	if request.Validate() != nil || current.ToolName != "" && current.ToolName != request.Action {
		return request, &registeredPreparation{err: operation.ErrIndependenceUnproven}, nil
	}
	return request, &registeredPreparation{plan: recoveryPlan{request: request, state: state}}, nil
}

// recoveryPlan is owned by one authorized worker lifetime. The accepted plan
// bytes stay immutable while preparation adds only execution metadata.
type recoveryPlan struct {
	request Request
	state   operation.RemoteJobState
	digest  string
}

func decodeRecoveryPlan(current operation.Operation) (recoveryPlan, error) {
	r, state, err := recoveryRequest(current)
	if err != nil {
		return recoveryPlan{}, err
	}
	return recoveryPlan{request: r, state: state}, nil
}

func (p *recoveryPlan) planDigest() string {
	if p.digest == "" {
		p.digest = basisDigest(p.state.Plan.Data)
	}
	return p.digest
}

func basisDigest(raw []byte) string { d := sha256.Sum256(raw); return hex.EncodeToString(d[:]) }
func (h *Handler) decodeBasis(raw []byte) (executionBasis, error) {
	var b executionBasis
	if len(raw) == 0 || len(raw) > 256<<10 || json.Unmarshal(raw, &b, json.RejectUnknownMembers(true)) != nil || b.Version != 1 || (b.Action != "write" && b.Action != "edit") || len(b.PlanSHA256) != 64 || b.Footprint.Root != h.executor.Files.Root() || len(b.Footprint.Targets) != 1 {
		return b, operation.ErrIndependenceUnproven
	}
	if _, err := hex.DecodeString(b.PlanSHA256); err != nil {
		return b, operation.ErrIndependenceUnproven
	}
	return b, nil
}

// PrepareExecution captures concrete metadata under the current operation
// lease. The worker must receive a durable owner acknowledgement before effects.
func (h *Handler) PrepareExecution(ctx context.Context, current operation.Operation) (operation.Operation, error) {
	plan, err := decodeRecoveryPlan(current)
	if err != nil {
		return current, err
	}
	return h.prepareExecutionWithPlan(ctx, current, &plan)
}

func (h *Handler) prepareExecutionWithPlan(ctx context.Context, current operation.Operation, plan *recoveryPlan) (operation.Operation, error) {
	r := plan.request
	if r.Action != "write" && r.Action != "edit" {
		return current, nil
	}
	if len(current.ExecutionBasis()) != 0 {
		b, err := h.decodeBasis(current.ExecutionBasis())
		path, pathErr := h.executor.Files.Path(r.Path)
		if err != nil || pathErr != nil || b.Action != r.Action || b.PlanSHA256 != plan.planDigest() || b.Footprint.Targets[0].Path != path {
			return current, operation.ErrIndependenceUnproven
		}
		if h.executor.Files.ValidateExecutionFootprint(ctx, b.Footprint) != nil {
			return current, operation.ErrIndependenceUnproven
		}
		return current, nil
	}
	f, err := h.executor.Files.CaptureFootprint(ctx, []string{r.Path})
	if err != nil {
		return current, err
	}
	b := executionBasis{Version: 1, Action: r.Action, PlanSHA256: plan.planDigest(), Footprint: f}
	raw, err := json.Marshal(b)
	if err != nil {
		return current, err
	}
	return operation.WithExecutionBasis(current, raw), nil
}

// PrepareRecovery verifies historical parent/root identities, actual finite
// executor read/write targets and expected revisions. Empty dependency lists or
// model assertions supply no positive evidence to this check.
func (h *Handler) PrepareRecovery(ctx context.Context, current operation.Operation, blocked []operation.UncertainEffect) (operation.Operation, error) {
	if len(blocked) == 0 || len(blocked) > 128 {
		return current, operation.ErrIndependenceUnproven
	}
	prepared, err := h.PrepareExecution(ctx, current)
	if err != nil {
		return current, operation.ErrIndependenceUnproven
	}
	r, _, err := recoveryRequest(prepared)
	if err != nil || (r.Action != "write" && r.Action != "edit") {
		return current, operation.ErrIndependenceUnproven
	}
	var paths []string
	historical := make([]mutation.Footprint, 0, len(blocked))
	seen := make(map[operation.ID]bool, len(blocked))
	for _, effect := range blocked {
		if effect.ID == "" || effect.ID == current.ID || seen[effect.ID] {
			return current, operation.ErrIndependenceUnproven
		}
		seen[effect.ID] = true
		b, err := h.decodeBasis(effect.Basis)
		if err != nil || h.executor.Files.ValidateFootprint(ctx, b.Footprint) != nil {
			return current, operation.ErrIndependenceUnproven
		}
		for _, target := range b.Footprint.Targets {
			paths = append(paths, target.Path)
		}
		historical = append(historical, b.Footprint)
	}
	ctx = mutation.WithBlockedFootprints(ctx, historical)
	request := mutation.Request{Version: mutation.Version, Changes: []mutation.Change{{Path: r.Path, Expected: *r.Expected, Content: []byte(r.Content)}}}
	if _, err = h.executor.Files.ProveIndependent(ctx, request, paths); err != nil {
		return current, operation.ErrIndependenceUnproven
	}
	return operation.WithRecoveryBasis(prepared, blocked), nil
}

func (h *Handler) recoveryExecutionContext(ctx context.Context, current operation.Operation) (context.Context, error) {
	return h.recoveryExecutionContextWithPlan(ctx, current, nil)
}

func (h *Handler) recoveryExecutionContextWithPlan(ctx context.Context, current operation.Operation, plan *recoveryPlan) (context.Context, error) {
	if len(current.ExecutionBasis()) != 0 {
		basis, err := h.decodeBasis(current.ExecutionBasis())
		if err != nil {
			return ctx, err
		}
		if plan == nil {
			decoded, err := decodeRecoveryPlan(current)
			if err != nil {
				return ctx, operation.ErrIndependenceUnproven
			}
			plan = &decoded
		}
		r := plan.request
		path, pathErr := h.executor.Files.Path(r.Path)
		if pathErr != nil || basis.Action != r.Action || basis.PlanSHA256 != plan.planDigest() || basis.Footprint.Targets[0].Path != path {
			return ctx, operation.ErrIndependenceUnproven
		}
		ctx = mutation.WithExecutionFootprint(ctx, basis.Footprint)
	}
	if len(current.RecoveryBasis()) == 0 {
		return ctx, nil
	}
	var paths []string
	var footprints []mutation.Footprint
	for _, effect := range current.RecoveryBasis() {
		b, err := h.decodeBasis(effect.Basis)
		if err != nil {
			return ctx, err
		}
		if err := h.executor.Files.ValidateFootprint(ctx, b.Footprint); err != nil {
			return ctx, err
		}
		footprints = append(footprints, b.Footprint)
		for _, target := range b.Footprint.Targets {
			paths = append(paths, target.Path)
		}
	}
	return mutation.WithIndependenceRequirement(mutation.WithBlockedFootprints(ctx, footprints), paths), nil
}

var _ operation.ExecutionPreparer = (*Handler)(nil)
var _ operation.RecoveryPreparer = (*Handler)(nil)
