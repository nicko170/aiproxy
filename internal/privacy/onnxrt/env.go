package onnxrt

import (
	"fmt"

	"github.com/nicko170/aiproxy/internal/privacy/onnxrt/internal/api"
)

// Env represents an ONNX Runtime environment that manages global state
// and configuration for all inference sessions.
type Env struct {
	ptr     api.OrtEnv
	runtime *Runtime
}

// NewEnv creates a new ONNX Runtime environment with the specified logging level and identifier.
// The logLevel parameter controls logging verbosity, and logID is used to tag log messages.
func (r *Runtime) NewEnv(logID string, logLevel LoggingLevel) (*Env, error) {
	logIDBytes := append([]byte(logID), 0)
	var envPtr api.OrtEnv

	status := r.apiFuncs.CreateEnv(logLevel, &logIDBytes[0], &envPtr)
	if err := r.statusError(status); err != nil {
		return nil, fmt.Errorf("failed to create environment: %w", err)
	}

	return &Env{
		ptr:     envPtr,
		runtime: r,
	}, nil
}

// Close releases the environment and frees associated resources.
func (e *Env) Close() {
	if e.ptr != 0 && e.runtime != nil && e.runtime.apiFuncs != nil {
		e.runtime.apiFuncs.ReleaseEnv(e.ptr)
		e.ptr = 0
	}
}
