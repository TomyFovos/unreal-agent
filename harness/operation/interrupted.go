package operation

import "fmt"

// CancelUndispatched closes a recovered operation without starting an external
// effect. It is only valid when its previous executor is no longer alive.
func CancelUndispatched(current Operation) (Operation, error) {
	var step Step
	var err error
	switch current.Type {
	case TypeValue:
		current.Status = StatusCanceled
		return current, nil
	case TypeShell:
		var actor *Shell
		actor, err = NewShell(current)
		if err == nil {
			step, err = actor.cancel()
		}
	case TypeViewImage:
		var actor *ViewImage
		actor, err = NewViewImage(current)
		if err == nil {
			step, err = actor.cancel()
		}
	case TypeSkillUse:
		canceled := failLocalOperation(current, fmt.Errorf("skill operation canceled during recovery"))
		canceled.Status = StatusCanceled
		return canceled, nil
	case TypeRemoteJob:
		step, err = CancelRemoteJob(current)
	default:
		return Operation{}, fmt.Errorf("cannot cancel recovered type %q: %w", current.Type, ErrUnsupported)
	}
	if err != nil {
		return Operation{}, err
	}
	if step.Operation == nil {
		return Operation{}, fmt.Errorf("cancellation has no checkpoint")
	}
	return *step.Operation, nil
}
