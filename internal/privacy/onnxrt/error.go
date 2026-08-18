package onnxrt

import (
	"fmt"
)

// RuntimeError represents an error returned from the ONNX Runtime C API.
type RuntimeError struct {
	Code    ErrorCode
	Message string
}

func (e *RuntimeError) Error() string {
	return fmt.Sprintf("onnxruntime error (code %d): %s", e.Code, e.Message)
}
