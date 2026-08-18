package onnxrt

import (
	"errors"

	"github.com/nicko170/aiproxy/internal/privacy/onnxrt/internal/api"
)

var (
	// ErrSessionClosed is returned when an operation is attempted on a closed session.
	ErrSessionClosed = errors.New("session is closed")
)

// ErrorCode represents error codes returned by the ONNX Runtime C API.
type ErrorCode = api.OrtErrorCode

// Error codes returned by the ONNX Runtime C API.
const (
	// ErrorCodeOK indicates success (no error).
	ErrorCodeOK ErrorCode = 0
	// ErrorCodeFail indicates a generic failure.
	ErrorCodeFail ErrorCode = 1
	// ErrorCodeInvalidArgument indicates an invalid argument was provided.
	ErrorCodeInvalidArgument ErrorCode = 2
	// ErrorCodeNoSuchFile indicates the specified file was not found.
	ErrorCodeNoSuchFile ErrorCode = 3
	// ErrorCodeNoModel indicates no model was loaded.
	ErrorCodeNoModel ErrorCode = 4
	// ErrorCodeEngineError indicates an error in the execution engine.
	ErrorCodeEngineError ErrorCode = 5
	// ErrorCodeRuntimeException indicates a runtime exception occurred.
	ErrorCodeRuntimeException ErrorCode = 6
	// ErrorCodeInvalidProtobuf indicates the protobuf format is invalid.
	ErrorCodeInvalidProtobuf ErrorCode = 7
	// ErrorCodeModelLoaded indicates the model is already loaded.
	ErrorCodeModelLoaded ErrorCode = 8
	// ErrorCodeNotImplemented indicates the feature is not implemented.
	ErrorCodeNotImplemented ErrorCode = 9
	// ErrorCodeInvalidGraph indicates the model graph is invalid.
	ErrorCodeInvalidGraph ErrorCode = 10
	// ErrorCodeEPFail indicates an execution provider failure.
	ErrorCodeEPFail ErrorCode = 11
)

// LoggingLevel represents logging verbosity levels for ONNX Runtime.
type LoggingLevel = api.OrtLoggingLevel

// Logging levels for ONNX Runtime.
const (
	// LoggingLevelVerbose enables verbose logging.
	LoggingLevelVerbose LoggingLevel = 0
	// LoggingLevelInfo enables informational logging.
	LoggingLevelInfo LoggingLevel = 1
	// LoggingLevelWarning enables warning logging.
	LoggingLevelWarning LoggingLevel = 2
	// LoggingLevelError enables error logging.
	LoggingLevelError LoggingLevel = 3
	// LoggingLevelFatal enables fatal error logging only.
	LoggingLevelFatal LoggingLevel = 4
)

// ONNXType represents the type of an ONNX value.
type ONNXType = api.ONNXType

// ONNX value types.
const (
	// ONNXTypeUnknown indicates an unknown type.
	ONNXTypeUnknown ONNXType = 0
	// ONNXTypeTensor indicates a tensor value.
	ONNXTypeTensor ONNXType = 1
	// ONNXTypeSequence indicates a sequence value.
	ONNXTypeSequence ONNXType = 2
	// ONNXTypeMap indicates a map value.
	ONNXTypeMap ONNXType = 3
	// ONNXTypeOpaque indicates an opaque value.
	ONNXTypeOpaque ONNXType = 4
	// ONNXTypeSparsetensor indicates a sparse tensor value.
	ONNXTypeSparsetensor ONNXType = 5
	// ONNXTypeOptional indicates an optional value.
	ONNXTypeOptional ONNXType = 6
)

// ONNXTensorElementDataType represents the data type of tensor elements.
type ONNXTensorElementDataType = api.ONNXTensorElementDataType

// Tensor element data types supported by ONNX.
const (
	// ONNXTensorElementDataTypeUndefined indicates an undefined data type.
	ONNXTensorElementDataTypeUndefined ONNXTensorElementDataType = 0
	// ONNXTensorElementDataTypeFloat indicates float32 data type.
	ONNXTensorElementDataTypeFloat ONNXTensorElementDataType = 1
	// ONNXTensorElementDataTypeUint8 indicates uint8 data type.
	ONNXTensorElementDataTypeUint8 ONNXTensorElementDataType = 2
	// ONNXTensorElementDataTypeInt8 indicates int8 data type.
	ONNXTensorElementDataTypeInt8 ONNXTensorElementDataType = 3
	// ONNXTensorElementDataTypeUint16 indicates uint16 data type.
	ONNXTensorElementDataTypeUint16 ONNXTensorElementDataType = 4
	// ONNXTensorElementDataTypeInt16 indicates int16 data type.
	ONNXTensorElementDataTypeInt16 ONNXTensorElementDataType = 5
	// ONNXTensorElementDataTypeInt32 indicates int32 data type.
	ONNXTensorElementDataTypeInt32 ONNXTensorElementDataType = 6
	// ONNXTensorElementDataTypeInt64 indicates int64 data type.
	ONNXTensorElementDataTypeInt64 ONNXTensorElementDataType = 7
	// ONNXTensorElementDataTypeString indicates string data type.
	ONNXTensorElementDataTypeString ONNXTensorElementDataType = 8
	// ONNXTensorElementDataTypeBool indicates boolean data type.
	ONNXTensorElementDataTypeBool ONNXTensorElementDataType = 9
	// ONNXTensorElementDataTypeFloat16 indicates float16 data type.
	ONNXTensorElementDataTypeFloat16 ONNXTensorElementDataType = 10
	// ONNXTensorElementDataTypeDouble indicates float64 data type.
	ONNXTensorElementDataTypeDouble ONNXTensorElementDataType = 11
	// ONNXTensorElementDataTypeUint32 indicates uint32 data type.
	ONNXTensorElementDataTypeUint32 ONNXTensorElementDataType = 12
	// ONNXTensorElementDataTypeUint64 indicates uint64 data type.
	ONNXTensorElementDataTypeUint64 ONNXTensorElementDataType = 13
	// ONNXTensorElementDataTypeComplex64 indicates complex64 data type.
	ONNXTensorElementDataTypeComplex64 ONNXTensorElementDataType = 14
	// ONNXTensorElementDataTypeComplex128 indicates complex128 data type.
	ONNXTensorElementDataTypeComplex128 ONNXTensorElementDataType = 15
)

// allocatorType represents memory allocator types.
type allocatorType = api.OrtAllocatorType

// Memory allocator types.
const (
	// allocatorTypeDevice indicates a device-specific allocator.
	allocatorTypeDevice allocatorType = 0
)

// memType represents memory types for allocations.
type memType = api.OrtMemType

// Memory types for allocations.
const (
	// memTypeCPU indicates general CPU memory.
	memTypeCPU memType = 0
)
