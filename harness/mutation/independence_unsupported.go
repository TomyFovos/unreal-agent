//go:build !linux && !darwin

package mutation

import "context"

func (*Service) captureIndependenceMetadata(context.Context, string) (independencePath, error) {
	return independencePath{}, ErrIndependenceUnproven
}

func (*Service) captureAuthorizedIndependence(context.Context, string, bool, bool) (independencePath, error) {
	return independencePath{}, ErrIndependenceUnproven
}
