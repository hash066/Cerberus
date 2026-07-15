//go:build windows || linux

package inference

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestLlamacppMockForwardRange(t *testing.T) {
	stage01, reported, err := ForwardRange(BackendLlamacpp, EncodeActivation(DefaultActivation), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if reported != llamacppMockBackend {
		t.Fatalf("backend = %q want %q", reported, llamacppMockBackend)
	}
	full, _, err := ForwardRange(BackendLlamacpp, EncodeActivation(DefaultActivation), 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	stage23, _, err := ForwardRange(BackendLlamacpp, stage01, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stage23, full) {
		t.Fatalf("chained shard mismatch")
	}
}

func TestLlamacppMockDistinctFromSplitMLP(t *testing.T) {
	split, _, err := ForwardRange(BackendCPUSoftware, EncodeActivation(DefaultActivation), 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	mock, _, err := ForwardRange(BackendLlamacpp, EncodeActivation(DefaultActivation), 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(split, mock) {
		t.Fatal("llamacpp mock output must differ from splitmlp fixture")
	}
}

func TestLlamacppReportedBackendHonestWhenMocked(t *testing.T) {
	if got := ReportedBackend(BackendLlamacpp); got != llamacppMockBackend {
		t.Fatalf("ReportedBackend = %q want %q", got, llamacppMockBackend)
	}
}

func TestLlamacppConnProtocol(t *testing.T) {
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	defer reqW.Close()

	go func() {
		defer respW.Close()
		sc := bufio.NewScanner(reqR)
		for sc.Scan() {
			var req llamacppRequest
			_ = json.Unmarshal(sc.Bytes(), &req)
			var resp llamacppResponse
			switch req.Op {
			case "ping":
				resp = llamacppResponse{OK: true}
			case "forward_layers":
				resp = llamacppResponse{
					OK:              true,
					Backend:         llamacppRealBackend,
					ActivationBytes: "AAAAAAA=", // invalid on purpose for shape test
					Shape:           []uint32{4},
					DType:           "f32",
				}
			default:
				resp = llamacppResponse{OK: false, Error: "unknown"}
			}
			b, _ := json.Marshal(resp)
			_, _ = respW.Write(append(b, '\n'))
		}
	}()

	conn := newLlamacppConn(reqW, respR)
	ping, err := conn.roundTrip(llamacppRequest{Op: "ping"})
	if err != nil || !ping.OK {
		t.Fatalf("ping: %+v %v", ping, err)
	}
}

func TestLlamacppHelperProcessProtocol(t *testing.T) {
	t.Setenv("CERBERUS_LLAMACPP_MOCK", "")
	// Helper readiness depends on python + llama-cpp-python, not model path.
	ready := LlamacppHelperReady()
	if ready {
		t.Log("llamacpp helper ready (llama-cpp-python importable)")
	} else {
		t.Log("llamacpp helper not ready — mock path will be used")
	}
}

func TestLlamacppRealForwardLayers(t *testing.T) {
	t.Setenv("CERBERUS_LLAMACPP_MOCK", "")
	model, err := ModelPathForTest()
	if err != nil {
		t.Skipf("skipping real forward: %v", err)
	}
	t.Setenv("CERBERUS_LLAMA_MODEL", model)

	if !LlamacppHelperReady() {
		t.Skip("llamacpp helper not ready (pip install llama-cpp-python and set CERBERUS_LLAMA_MODEL)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	act := ActivationFromSplitMLP(DefaultActivation)
	out, reported, err := ForwardActivation(BackendLlamacpp, act, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if reported != llamacppRealBackend {
		t.Fatalf("backend = %q want %q", reported, llamacppRealBackend)
	}
	if len(out.Payload) == 0 {
		t.Fatal("expected non-empty activation payload")
	}
	_ = ctx
}

func TestActivationWireRoundTrip(t *testing.T) {
	raw, err := EncodeActivationWire([]float32{1, 2, 3, 4}, []uint32{4})
	if err != nil {
		t.Fatal(err)
	}
	vals, shape, err := DecodeActivationWire(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 4 || shape[0] != 4 {
		t.Fatalf("got %v shape %v", vals, shape)
	}
}

func TestLlamacppAvailableOnWindowsOrLinux(t *testing.T) {
	if !LlamacppAvailable() {
		t.Fatal("expected llamacpp supported on windows/linux build")
	}
}

func TestCompleteWithoutModelErrors(t *testing.T) {
	t.Setenv("CERBERUS_LLAMACPP_MOCK", "")
	t.Setenv("CERBERUS_LLAMA_MODEL", "")
	t.Setenv("LLAMA_MODEL", "")
	_, _, err := Complete(t.Context(), "hello", 1)
	if err == nil {
		t.Fatal("expected error without model path")
	}
	if !strings.Contains(err.Error(), "model") {
		t.Fatalf("expected model error, got: %v", err)
	}
}
