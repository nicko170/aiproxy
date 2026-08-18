package onnxrt

import (
	"unsafe"

	"github.com/nicko170/aiproxy/internal/privacy/onnxrt/internal/api"
)

// allocator represents an ONNX Runtime allocator (internal use only)
type allocator struct {
	ptr     api.OrtAllocator
	runtime *Runtime
}

// free frees memory allocated by the allocator (internal use)
func (a *allocator) free(ptr unsafe.Pointer) {
	if a.runtime == nil || a.runtime.apiFuncs == nil || ptr == nil {
		return
	}

	a.runtime.apiFuncs.AllocatorFree(a.ptr, ptr)
}

// memoryInfo represents ONNX Runtime memory information (internal use only)
type memoryInfo struct {
	ptr     api.OrtMemoryInfo
	runtime *Runtime
}

// release releases the memory info (internal use)
func (mi *memoryInfo) release() {
	if mi.ptr != 0 && mi.runtime != nil && mi.runtime.apiFuncs != nil {
		mi.runtime.apiFuncs.ReleaseMemoryInfo(mi.ptr)
		mi.ptr = 0
	}
}
