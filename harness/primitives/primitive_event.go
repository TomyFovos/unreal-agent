package primitives

import "github.com/unreallabsai/unreal-agent/harness/permission"

type PrimitiveEventType string

type SourceID string

type CorrelationID string

const (
	PrimitiveEventFailed   PrimitiveEventType = "primitive.failed"
	PrimitiveEventCanceled PrimitiveEventType = "primitive.canceled"
)

type PrimitiveEvent struct {
	Type          PrimitiveEventType
	Source        SourceID
	CorrelationID CorrelationID
	Result        any
}

type PrimitiveFailureResult struct {
	Denial *permission.Error
	Error  string
}

func primitiveFailure(
	source SourceID,
	correlationID CorrelationID,
	err error,
) PrimitiveEvent {
	if denial := permission.Failure(err); denial != nil {
		err = denial
	}
	return PrimitiveEvent{
		Type:          PrimitiveEventFailed,
		Source:        source,
		CorrelationID: correlationID,
		Result: PrimitiveFailureResult{
			Error:  err.Error(),
			Denial: permission.Failure(err),
		},
	}
}

func primitiveCanceled(
	source SourceID,
	correlationID CorrelationID,
) PrimitiveEvent {
	return PrimitiveEvent{
		Type:          PrimitiveEventCanceled,
		Source:        source,
		CorrelationID: correlationID,
	}
}
