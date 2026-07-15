package inference

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseBackendMLX(t *testing.T) {
	got, err := ParseBackend("mlx")
	if err != nil {
		t.Fatal(err)
	}
	if got != BackendMLX {
		t.Fatalf("got %q want %q", got, BackendMLX)
	}
}

func TestMLXComponentTagRoundTrip(t *testing.T) {
	tag := ComponentTag(BackendMLX)
	if !IsPipelineComponent(tag) {
		t.Fatalf("%q not recognized as pipeline component", tag)
	}
	if BackendFromComponent(tag) != BackendMLX {
		t.Fatal("round-trip failed")
	}
}

// TestMLXMockForwardMatchesSplitMLP: the mlx mock fallback runs the SAME
// deterministic split-MLP weights the real sidecar computes with mlx arrays,
// so mock output must equal the cpu-software fixture — only the reported
// backend string differs (and never lies).
func TestMLXMockForwardMatchesSplitMLP(t *testing.T) {
	t.Setenv("CERBERUS_MLX_MOCK", "1")

	mlxOut, reported, err := ForwardRange(BackendMLX, EncodeActivation(DefaultActivation), 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if reported != mlxMockBackend {
		t.Fatalf("backend = %q want %q", reported, mlxMockBackend)
	}
	cpuOut, _, err := ForwardRange(BackendCPUSoftware, EncodeActivation(DefaultActivation), 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mlxOut, cpuOut) {
		t.Fatal("mlx mock must match the split-MLP fixture (identical weights)")
	}
}

func TestMLXMockForwardChainsAcrossShards(t *testing.T) {
	t.Setenv("CERBERUS_MLX_MOCK", "1")

	stage01, _, err := ForwardRange(BackendMLX, EncodeActivation(DefaultActivation), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	stage23, _, err := ForwardRange(BackendMLX, stage01, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	full, _, err := ForwardRange(BackendMLX, EncodeActivation(DefaultActivation), 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stage23, full) {
		t.Fatal("chained shard mismatch")
	}
}

func TestMLXForwardRangeRejectsBadRange(t *testing.T) {
	t.Setenv("CERBERUS_MLX_MOCK", "1")
	if _, _, err := ForwardRange(BackendMLX, EncodeActivation(DefaultActivation), 2, 1); err == nil {
		t.Fatal("expected error for inverted range")
	}
	// Extended layer ranges (e.g. 22-layer LLM shards) use mockForwardRange in mock mode.
	if _, _, err := ForwardRange(BackendMLX, EncodeActivation(DefaultActivation), 0, 7); err != nil {
		t.Fatalf("mock mlx should accept extended layer ranges: %v", err)
	}
}

func TestMLXReportedBackendHonestWhenMocked(t *testing.T) {
	t.Setenv("CERBERUS_MLX_MOCK", "1")
	if got := ReportedBackend(BackendMLX); got != mlxMockBackend {
		t.Fatalf("ReportedBackend = %q want %q", got, mlxMockBackend)
	}
}

// TestMLXConnProtocol exercises the JSON-lines round trip against an
// in-memory fake sidecar — real protocol coverage with no Python involved.
func TestMLXConnProtocol(t *testing.T) {
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	defer reqW.Close()
	defer respW.Close()

	// Fake sidecar: echoes splitmlp activation doubled; forward_layers adds shape metadata.
	go func() {
		sc := bufio.NewScanner(reqR)
		for sc.Scan() {
			var req mlxRequest
			if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
				t.Errorf("fake sidecar decode: %v", err)
				return
			}
			var resp mlxResponse
			switch req.Op {
			case "forward_splitmlp":
				resp.OK = true
				resp.Backend = "mlx"
				for _, f := range req.Activation {
					resp.Activation = append(resp.Activation, f*2)
				}
			case "forward_layers":
				resp.OK = true
				resp.Backend = "mlx"
				resp.Shape = []uint32{4}
				resp.DType = "f32"
				resp.ActivationBytes = req.ActivationBytes
				if req.ActivationBytes == "" {
					resp.ActivationBytes = "AAAAAA==" // placeholder
				}
			default:
				resp.Error = "unknown op " + req.Op
			}
			b, _ := json.Marshal(resp)
			if _, err := respW.Write(append(b, '\n')); err != nil {
				return
			}
		}
	}()

	conn := newMLXConn(reqW, respR)
	resp, err := conn.roundTrip(mlxRequest{
		Op: "forward_splitmlp", LayerLo: 0, LayerHi: 1, Activation: []float32{1, 2, 3, 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Backend != "mlx" {
		t.Fatalf("resp = %+v", resp)
	}
	want := []float32{2, 4, 6, 8}
	for i, f := range resp.Activation {
		if f != want[i] {
			t.Fatalf("activation[%d] = %v want %v", i, f, want[i])
		}
	}

	layersResp, err := conn.roundTrip(mlxRequest{
		Op:              "forward_layers",
		LayerLo:         0,
		LayerHi:         1,
		ActivationBytes: "AAAAAA==",
		Shape:           []uint32{1},
		DType:           "i32",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !layersResp.OK || layersResp.Backend != "mlx" || len(layersResp.Shape) == 0 {
		t.Fatalf("forward_layers resp = %+v", layersResp)
	}

	errResp, err := conn.roundTrip(mlxRequest{Op: "bogus"})
	if err != nil {
		t.Fatal(err)
	}
	if errResp.OK || !strings.Contains(errResp.Error, "unknown op") {
		t.Fatalf("error resp = %+v", errResp)
	}
}

// TestMLXSidecarProcessProtocol spawns the REAL sidecar.py under whatever
// Python is on this machine and drives ping/info/forward_splitmlp. It skips
// when no interpreter is present. On a machine without the mlx pip package
// (any non-Apple-Silicon box) it asserts the sidecar's honest failure mode;
// with mlx installed it asserts real values against the Go fixture.
func TestMLXSidecarProcessProtocol(t *testing.T) {
	if _, err := resolveMLXPython(); err != nil {
		t.Skipf("no python interpreter: %v", err)
	}
	sc, err := startMLXSidecar()
	if err != nil {
		t.Skipf("sidecar did not start (python too old / unavailable?): %v", err)
	}
	defer sc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	info, err := sc.call(ctx, mlxRequest{Op: "info"})
	if err != nil {
		t.Fatal(err)
	}
	if !info.OK {
		t.Fatalf("info: %+v", info)
	}

	fwd, err := sc.call(ctx, mlxRequest{
		Op: "forward_splitmlp", LayerLo: 0, LayerHi: 3,
		Activation: DefaultActivation[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	if !info.MLX {
		// No mlx on this machine: the sidecar must say so, not fake numbers.
		if fwd.OK {
			t.Fatal("forward_splitmlp succeeded without mlx installed?")
		}
		if !strings.Contains(fwd.Error, "mlx not importable") {
			t.Fatalf("expected honest mlx-missing error, got %q", fwd.Error)
		}
		return
	}
	// Real mlx: values must match the deterministic Go fixture (float tolerance).
	if !fwd.OK {
		t.Fatalf("forward_splitmlp: %s", fwd.Error)
	}
	want := splitMLPExpectedOutput()
	for i := range want {
		if diff := math.Abs(float64(fwd.Activation[i] - want[i])); diff > 1e-4 {
			t.Fatalf("activation[%d] = %v want %v (diff %v)", i, fwd.Activation[i], want[i], diff)
		}
	}
}

// TestMLXRealForwardPass runs one real forward pass on the configured model.
// It only runs on macOS/Apple Silicon with mlx+mlx-lm installed (and network
// access for the first model download); everywhere else it skips.
func TestMLXRealForwardPass(t *testing.T) {
	if !MLXSidecarReady() {
		t.Skip("mlx sidecar not ready (requires macOS on Apple Silicon with `pip install mlx mlx-lm`)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := MLXForwardPass(ctx, "The capital of France is")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.TopTokens) == 0 {
		t.Fatal("expected top next-token logits")
	}
	if res.NLayers == 0 || res.HiddenSize == 0 {
		t.Fatalf("missing model shape info: %+v", res)
	}
	if res.Backend != mlxRealBackend {
		t.Fatalf("backend = %q", res.Backend)
	}
	t.Logf("model: %d layers, hidden %d; top tokens: %+v", res.NLayers, res.HiddenSize, res.TopTokens)
}

// TestMLXRealForwardLayers runs forward_layers on the configured model.
// Requires macOS/Apple Silicon, mlx+mlx-lm, CERBERUS_MLX_MODEL, and network
// on first download; skips everywhere else.
func TestMLXRealForwardLayers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real MLX forward_layers in -short mode")
	}
	t.Setenv("CERBERUS_MLX_MOCK", "")
	if os.Getenv("CERBERUS_MLX_MODEL") == "" {
		t.Setenv("CERBERUS_MLX_MODEL", "mlx-community/Llama-3.2-1B-Instruct-4bit")
	}
	if !MLXSidecarReady() {
		t.Skip("mlx sidecar not ready (requires macOS on Apple Silicon with `pip install mlx mlx-lm`)")
	}
	mlxMu.Lock()
	hasLM := mlxHasMLXLM
	mlxMu.Unlock()
	if !hasLM {
		t.Skip("mlx-lm not importable in sidecar")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Layer 0 shard: legacy split-MLP bytes trigger prompt tokenization in sidecar.
	in := ActivationFromSplitMLP(DefaultActivation)
	out, reported, err := ForwardActivation(BackendMLX, in, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if reported != mlxRealBackend {
		t.Fatalf("backend = %q want %q", reported, mlxRealBackend)
	}
	if len(out.Shape) != 2 {
		t.Fatalf("expected hidden shape [seq, hidden], got %v", out.Shape)
	}
	if len(out.Payload) == 0 {
		t.Fatal("expected non-empty hidden payload")
	}
	t.Logf("layer-0 shard: shape=%v payload=%d bytes", out.Shape, len(out.Payload))

	// Chain one more layer if the model has more than one.
	sc, err := sharedMLXSidecar()
	if err != nil {
		t.Fatal(err)
	}
	info, err := sc.call(ctx, mlxRequest{Op: "info"})
	if err != nil {
		t.Fatal(err)
	}
	_ = info
	mid, reported, err := ForwardActivation(BackendMLX, out, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if reported != mlxRealBackend {
		t.Fatalf("backend = %q", reported)
	}
	if len(mid.Payload) == 0 {
		t.Fatal("expected mid-layer payload")
	}
	t.Logf("layer-1 shard: shape=%v payload=%d bytes", mid.Shape, len(mid.Payload))
}

func TestMLXGenerateUnavailableIsClear(t *testing.T) {
	if MLXAvailable() {
		t.Skip("darwin build: generate may genuinely work here")
	}
	_, _, err := MLXGenerate(context.Background(), "hello", 4)
	if err == nil {
		t.Fatal("expected error on non-darwin platform")
	}
	if !strings.Contains(err.Error(), "unsupported platform") {
		t.Fatalf("expected honest platform error, got: %v", err)
	}
}
