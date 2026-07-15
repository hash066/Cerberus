package gateway

import (
	"context"

	"github.com/hash066/cerberus/daemon/system"
)

// SystemInference adapts system.InferenceService to the gateway InferenceRunner
// interface without pulling system types into the gateway handler surface.
type SystemInference struct {
	Svc *system.InferenceService
}

func (a *SystemInference) Run(ctx context.Context, subject, modelID string, input []byte) (string, InferenceMeta, error) {
	if a == nil || a.Svc == nil {
		return "", InferenceMeta{}, errInferenceUnavailable
	}
	res, err := a.Svc.Run(ctx, subject, modelID, input)
	meta := InferenceMeta{Backend: res.Backend, Node: res.Node}
	if err != nil {
		return "", meta, err
	}
	return res.Content, meta, nil
}

func (a *SystemInference) RunStream(ctx context.Context, subject, modelID string, input []byte, emit func(token string) error) (InferenceMeta, error) {
	if a == nil || a.Svc == nil {
		return InferenceMeta{}, errInferenceUnavailable
	}
	res, err := a.Svc.RunStream(ctx, subject, modelID, input, emit)
	meta := InferenceMeta{Backend: res.Backend, Node: res.Node}
	return meta, err
}

var errInferenceUnavailable = inferenceError("inference runner not available")

type inferenceError string

func (e inferenceError) Error() string { return string(e) }
