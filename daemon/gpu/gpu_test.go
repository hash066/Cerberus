package gpu

import "testing"

func equalF32(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDispatchKernels proves each kernel computes the correct result on whatever
// backend the build selects (pure-Go software in the default build). The values
// are integer-valued so the f32 comparison is exact and matches core/runtime.
func TestDispatchKernels(t *testing.T) {
	out, backend, err := Dispatch(VectorAdd, 0, []float32{1, 2, 3, 4}, []float32{10, 20, 30, 40})
	if err != nil {
		t.Fatalf("vector-add: %v", err)
	}
	if !equalF32(out, []float32{11, 22, 33, 44}) {
		t.Fatalf("vector-add = %v", out)
	}
	if backend == "" {
		t.Fatal("Dispatch must report the backend that ran")
	}

	out, _, err = Dispatch(Saxpy, 2, []float32{1, 2, 3}, []float32{0.5, 0.5, 0.5}) // 2*x + y
	if err != nil {
		t.Fatalf("saxpy: %v", err)
	}
	if !equalF32(out, []float32{2.5, 4.5, 6.5}) {
		t.Fatalf("saxpy = %v", out)
	}

	out, _, err = Dispatch(ScalarMul, 3, []float32{1, -2, 3.5}, nil) // x * 3
	if err != nil {
		t.Fatalf("scalar-mul: %v", err)
	}
	if !equalF32(out, []float32{3, -6, 10.5}) {
		t.Fatalf("scalar-mul = %v", out)
	}
}

func TestDispatchRejectsBadRequests(t *testing.T) {
	if _, _, err := Dispatch(VectorAdd, 0, []float32{1, 2}, []float32{1}); err == nil {
		t.Fatal("mismatched input lengths must error")
	}
	if _, _, err := Dispatch(Kernel(99), 0, []float32{1}, []float32{1}); err == nil {
		t.Fatal("unknown kernel must error")
	}
}

func TestParseKernel(t *testing.T) {
	cases := map[string]Kernel{"vector-add": VectorAdd, "saxpy": Saxpy, "scalar-mul": ScalarMul}
	for name, want := range cases {
		if k, ok := ParseKernel(name); !ok || k != want {
			t.Fatalf("ParseKernel(%q) = %v, %v; want %v", name, k, ok, want)
		}
	}
	if _, ok := ParseKernel("nope"); ok {
		t.Fatal("unknown kernel name must not parse")
	}
}
