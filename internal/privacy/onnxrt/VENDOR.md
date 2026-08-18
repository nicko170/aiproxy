# Vendored: onnxruntime-purego

Source: https://github.com/shota3506/onnxruntime-purego
Commit: 8db8bd7424b218de9ca0ebd894da8e86f5dc7983 (2026-03-16)
License: MIT (see LICENSE)

Vendored rather than imported because upstream states its API may change without
notice, and this is load-bearing for a privacy filter: an unannounced signature
change should be a merge conflict we resolve deliberately, not a build break on
someone else's schedule.

It reaches ONNX Runtime through purego, so `CGO_ENABLED=0` still holds — which is
what keeps the release workflow cross-compiling four targets from one runner, and
keeps install.sh and the self-updater working.

## Local changes

- Package renamed `onnxruntime` -> `onnxrt` (package clause in every top-level
  file), matching the directory it now lives in: `internal/privacy/onnxrt`.
- Tests and GenAI support removed; only session creation and Run are used.
  Dropped entirely from the vendored copy: `genai/`, `examples/`, `tools/`,
  `internal/tests/` (a large ONNX conformance harness with its own test model
  and data-loading code), and every `*_test.go` file under `onnxruntime/`.
  What remains is exactly the top-level package (env, runtime, session, value,
  allocator, error, version, doc) plus the two internal packages it depends on:
  `internal/api` (the opaque handle types and the `APIFuncs` interface — this
  interface already lists only the C API entry points the classifier needs:
  env/session/value lifecycle and `Run`, nothing from training, graph editing,
  or execution-provider-specific option structs) and `internal/api/v23` (the
  purego-registered function pointers for ONNX Runtime's C API version 23,
  code-generated upstream from `onnxruntime_c_api.h`).
- Import paths rewritten from `github.com/shota3506/onnxruntime-purego/...` to
  `github.com/nicko170/aiproxy/internal/privacy/onnxrt/...` so the vendored
  copy resolves within this module instead of requiring the upstream module.
- `internal/api/v23/api.go` declares the full 381-field `API` struct (a memory
  overlay of ONNX Runtime's `OrtApi` C struct) even though `funcs.go` only
  registers ~30 of those fields. This was deliberately left untrimmed: the
  struct's correctness depends on every field before the last one used
  (`SessionOptionsAppendExecutionProvider`, field 216) being present, in
  order, with the right pointer-sized type, or every offset after the first
  mistake reads the wrong function pointer — and a wrong function pointer here
  is a segfault in a loaded native library, which Go cannot recover from. The
  380 lines of unused trailing declarations are a lower risk than hand-editing
  a struct where correctness is "byte-for-byte offset match with a C header we
  did not write."
- Added `github.com/ebitengine/purego` as a genuine go.mod dependency (it was
  not already present in this module). This is the one part of "go.mod gains
  nothing" that could not hold: purego is not vendored, organizational glue
  code, it is the FFI mechanism itself — per-OS, per-architecture `dlopen` and
  calling-convention trampolines (including hand-written assembly). Vendoring
  that would mean re-deriving and maintaining raw syscall trampolines in this
  repo, which is a materially larger correctness risk than depending on a
  widely used library (it backs Ebitengine and is used by wazero, among
  others). `go get` resolved v0.10.2; upstream's own go.mod pins v0.9.0, and
  nothing in the vendored code depends on anything newer, so this is a
  same-minor-line bump rather than tracking an unrelated major version.
