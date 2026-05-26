# Vendored: supertonic Go engine

- **Source:** https://github.com/supertone-inc/supertonic — `go/helper.go`
- **Commit:** dff55dc00064c398736080c78195f577527832ae
- **License:** MIT (see ./LICENSE, copied verbatim from the upstream repo root;
  confirmed 2026-05-26). Vendoring permitted with the copyright notice + license text.
- **Pristine helper.go sha256:** 7a1b6454c7bc7504eb14f97b8d45a7beef4a4465ceb17fc670526cdfcd19c3c0
- **Vendored helper.go sha256:** 31793011166df7c4647c1bf220ebf3e29bd9d03aa1bac3d0b67e82dcdccdcf5e

## Local modifications (and only these)

1. Package clause: `package main` → `package supertonic_native`.
2. Upstream `go vet` defect: `fmt.Println("Using CPU for inference\n")` →
   `fmt.Println("Using CPU for inference")` (redundant trailing newline; behaviour unchanged).
3. Seed hook: `sampleNoisyLatent` reseeds from `tts.SeedFunc()` instead of a
   hardcoded `time.Now().UnixNano()`; `TextToSpeech.SeedFunc` is added and
   defaults to time-based in `LoadTextToSpeech` (see Task 2). Production
   behaviour is unchanged; evals pin the seed for reproducibility.
4. `gofmt -w` applied (upstream file was not canonically formatted).

No other lines differ from upstream. `scripts/verify-vendor.sh` enforces this.

## ONNX Runtime native library

Inference needs the ONNX Runtime C library at runtime, located via
`ONNXRUNTIME_LIB_PATH` (default fallback `/usr/local/lib/libonnxruntime.so`).
The Docker image bundles v1.16.0 aarch64. `InitializeONNXRuntime()` (in this
package) performs `ort.SetSharedLibraryPath` + `ort.InitializeEnvironment`.
