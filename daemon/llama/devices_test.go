package llama

import (
	"strings"
	"testing"
	"time"

	"github.com/hash066/cerberus/daemon/gpu"
)

// realListDevices is VERBATIM stdout from
// `llama-server.exe --list-devices` (llama.cpp b10021, Vulkan pack) on this repo's
// Windows test box on 2026-07-16. Do not tidy it: the "Intel(R) Iris(R)" name
// carries parentheses, which is exactly what the parser has to survive.
const realListDevices = `Available devices:
  Vulkan0: Intel(R) Iris(R) Xe Graphics (8001 MiB, 7345 MiB free)
  Vulkan1: NVIDIA GeForce RTX 3050 Laptop GPU (3964 MiB, 3369 MiB free)
`

func realDevices(t *testing.T) []GGMLDevice {
	t.Helper()
	devs := parseListDevices(realListDevices)
	if len(devs) != 2 {
		t.Fatalf("fixture did not parse: got %d devices, want 2", len(devs))
	}
	return devs
}

func TestParseListDevices_Real(t *testing.T) {
	devs := realDevices(t)

	// Vulkan0 is the INTEGRATED GPU. This ordering is the whole point of the file.
	if devs[0].ID != "Vulkan0" {
		t.Errorf("devs[0].ID = %q, want Vulkan0", devs[0].ID)
	}
	if devs[0].Name != "Intel(R) Iris(R) Xe Graphics" {
		t.Errorf("devs[0].Name = %q — the parenthesised vendor name must survive intact", devs[0].Name)
	}
	if devs[0].TotalMiB != 8001 || devs[0].FreeMiB != 7345 {
		t.Errorf("devs[0] memory = %d/%d MiB, want 8001/7345", devs[0].FreeMiB, devs[0].TotalMiB)
	}
	if devs[1].ID != "Vulkan1" || devs[1].Name != "NVIDIA GeForce RTX 3050 Laptop GPU" {
		t.Errorf("devs[1] = %+v, want Vulkan1 / RTX 3050", devs[1])
	}
	if devs[1].TotalMiB != 3964 || devs[1].FreeMiB != 3369 {
		t.Errorf("devs[1] memory = %d/%d MiB, want 3369/3964", devs[1].FreeMiB, devs[1].TotalMiB)
	}
}

func TestParseListDevices_SkipsGarbageNeverInvents(t *testing.T) {
	// A row that does not parse must be dropped, not defaulted into existence.
	devs := parseListDevices(`Available devices:
  Vulkan0: Something (not a memory spec)
  Vulkan1: NVIDIA GeForce RTX 3050 Laptop GPU (3964 MiB, 3369 MiB free)
  [N/A]
`)
	if len(devs) != 1 || devs[0].ID != "Vulkan1" {
		t.Fatalf("got %+v, want only the one parsable device", devs)
	}
}

// TestDefaultDevice_IndexMismatch is the regression test for "lends the wrong card".
//
// nvidia-smi calls the RTX 3050 index 0. Vulkan calls it Vulkan1 and gives index 0
// to the Intel iGPU. Anything that maps the probe's Index onto "Vulkan<N>" lends the
// iGPU while telemetry advertises the 3050's VRAM. The correct answer is Vulkan1.
func TestDefaultDevice_ResolvesByNameNotIndex(t *testing.T) {
	devs := realDevices(t)
	snap := gpu.VRAM{
		Source: "nvidia-smi",
		AsOf:   time.Now(),
		Devices: []gpu.Device{
			// nvidia-smi enumerates ONLY NVIDIA cards, so the 3050 is its index 0.
			{Index: 0, Name: "NVIDIA GeForce RTX 3050 Laptop GPU",
				TotalBytes: 4096 * 1024 * 1024, FreeBytes: 3951 * 1024 * 1024},
		},
	}

	got, why, ok := DefaultDevice(devs, snap)
	if !ok {
		t.Fatal("DefaultDevice returned not-ok with a measured NVIDIA GPU present")
	}
	if got.ID != "Vulkan1" {
		t.Fatalf("DefaultDevice picked %q (%s) — want Vulkan1, the RTX 3050.\n"+
			"Picking Vulkan0 here means the daemon lends the Intel iGPU while telemetry "+
			"advertises the 3050's VRAM.", got.ID, got.Name)
	}
	if !strings.Contains(why, "telemetry advertises") {
		t.Errorf("reason %q should explain the telemetry-consistency choice", why)
	}
}

// The iGPU has far MORE free memory (7345 MiB vs 3369 MiB). "Most free" must not be
// allowed to beat "the card telemetry actually advertises".
func TestDefaultDevice_DoesNotPreferRoomierIGPU(t *testing.T) {
	devs := realDevices(t)
	snap := gpu.VRAM{
		Source: "nvidia-smi", AsOf: time.Now(),
		Devices: []gpu.Device{{Index: 0, Name: "NVIDIA GeForce RTX 3050 Laptop GPU",
			TotalBytes: 4096 * 1024 * 1024, FreeBytes: 3951 * 1024 * 1024}},
	}
	got, _, ok := DefaultDevice(devs, snap)
	if !ok || got.ID != "Vulkan1" {
		t.Fatalf("got %+v — the iGPU's larger free pool must not win over the advertised dGPU", got)
	}
}

func TestDefaultDevice_UnknownVRAMFallsBackToGGMLView(t *testing.T) {
	devs := realDevices(t)
	got, why, ok := DefaultDevice(devs, gpu.VRAM{Source: gpu.SourceUnknown, AsOf: time.Now()})
	if !ok {
		t.Fatal("want a choice from ggml's own view when telemetry is blind")
	}
	// With no telemetry to be consistent with, most-free is all we have.
	if got.ID != "Vulkan0" {
		t.Errorf("got %q, want Vulkan0 (most free) when VRAM is unknown", got.ID)
	}
	if !strings.Contains(why, "could not measure") {
		t.Errorf("reason %q must admit telemetry was blind", why)
	}
}

func TestDefaultDevice_NoDevices(t *testing.T) {
	if _, _, ok := DefaultDevice(nil, gpu.VRAM{Source: gpu.SourceUnknown}); ok {
		t.Fatal("DefaultDevice must not invent a device when ggml sees none")
	}
}

// A card telemetry can see but ggml cannot must not be silently swapped for another.
func TestDefaultDevice_NameMismatchIsAdmitted(t *testing.T) {
	devs := realDevices(t)
	snap := gpu.VRAM{
		Source: "nvidia-smi", AsOf: time.Now(),
		Devices: []gpu.Device{{Index: 0, Name: "NVIDIA GeForce RTX 4090",
			TotalBytes: 24576 * 1024 * 1024, FreeBytes: 24000 * 1024 * 1024}},
	}
	_, why, ok := DefaultDevice(devs, snap)
	if !ok {
		t.Fatal("want a fallback choice rather than no worker at all")
	}
	if !strings.Contains(why, "disagree") {
		t.Errorf("reason %q must admit telemetry and ggml disagree", why)
	}
}

func TestSelectDevice_RejectsBareIndex(t *testing.T) {
	devs := realDevices(t)
	// "1" is the trap: it means the 3050 to nvidia-smi and the 3050 to Vulkan only by
	// coincidence — "0" means opposite cards in the two orderings.
	for _, want := range []string{"0", "1"} {
		_, err := SelectDevice(devs, want)
		if err == nil {
			t.Fatalf("SelectDevice(%q) must be refused: a bare index is ambiguous", want)
		}
		if !strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("error for %q = %v; must explain the ambiguity", want, err)
		}
	}
}

func TestSelectDevice_RejectsUnknownAndListsReal(t *testing.T) {
	devs := realDevices(t)
	_, err := SelectDevice(devs, "Vulkan7")
	if err == nil {
		t.Fatal("unknown device must be refused at startup, not at a peer's first session")
	}
	if !strings.Contains(err.Error(), "Vulkan1") {
		t.Errorf("error must show the real device table, got: %v", err)
	}
}

func TestSelectDevice_AcceptsIDAndList(t *testing.T) {
	devs := realDevices(t)
	for _, tc := range []struct{ in, want string }{
		{"Vulkan1", "Vulkan1"},
		{"vulkan1", "Vulkan1"},                 // case-insensitive, canonicalised
		{" Vulkan1 ", "Vulkan1"},               // trimmed
		{"Vulkan0,Vulkan1", "Vulkan0,Vulkan1"}, // explicit multi-device opt-in
	} {
		got, err := SelectDevice(devs, tc.in)
		if err != nil {
			t.Fatalf("SelectDevice(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("SelectDevice(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The -d value must reach the argv exactly as resolved.
func TestRPCServerArgs_CarriesResolvedDevice(t *testing.T) {
	args := rpcServerArgs(RPCServerConfig{Port: 50052, Device: "Vulkan1"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-d Vulkan1") {
		t.Fatalf("argv %q must carry -d Vulkan1", joined)
	}
}
