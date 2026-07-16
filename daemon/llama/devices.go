package llama

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hash066/cerberus/daemon/gpu"
)

// devices.go answers "which GPU is this node lending?" — and refuses to answer it
// with an integer.
//
// # The index mismatch (measured on this repo's Windows test box, 2026-07-16)
//
// There are two device orderings in play and they DO NOT line up:
//
//	nvidia-smi (daemon/gpu.Probe):  index 0 = NVIDIA GeForce RTX 3050 Laptop GPU
//	ggml/Vulkan (-d):              Vulkan0 = Intel(R) Iris(R) Xe Graphics
//	                               Vulkan1 = NVIDIA GeForce RTX 3050 Laptop GPU
//
// The mismatch is STRUCTURAL, not a quirk of one machine: nvidia-smi enumerates
// only NVIDIA GPUs, while Vulkan enumerates every Vulkan-capable device including
// the integrated one. On any laptop with an iGPU and one NVIDIA dGPU — the single
// most common discrete-GPU configuration on earth — nvidia-smi's index 0 is the
// dGPU and Vulkan's index 0 is the iGPU. They are never safely interchangeable.
//
// So mapping gpu.Device.Index to a "-d Vulkan<N>" string would lend the Intel iGPU
// while telemetry advertises the RTX 3050's VRAM: the node would accept work sized
// for a 4 GiB dGPU and run it on the iGPU. That is the "lends the wrong card" bug.
// This file exists to make that mapping impossible: resolution is BY NAME, and a
// bare integer is rejected with an explanation rather than guessed at.
//
// The two sources also disagree NUMERICALLY, which is the other reason not to match
// them up by memory size: for the same RTX 3050, nvidia-smi reports 4096 MiB total
// while Vulkan reports 3964 MiB. They measure different things (board memory vs the
// heap budget Vulkan exposes). Only the NAME is common ground.
//
// # Why llama-server --list-devices is the source
//
// It is upstream's own supported enumeration, it prints to stdout, exits 0, and it
// reports exactly the ggml device IDs that -d accepts — from the same backend
// registry and the same DLLs that ggml-rpc-server loads out of the same pack dir.
// Verified at b10021: ggml-rpc-server's own startup banner lists the identical IDs.
// Asking the binaries that will run the work beats maintaining a parallel guess.

// GGMLDevice is one accelerator as ggml itself names it.
type GGMLDevice struct {
	// ID is ggml's device identifier ("Vulkan0", "Vulkan1", "CUDA0"). This — never
	// an integer — is what RPCServerConfig.Device / -d takes.
	ID string
	// Name is the vendor string ("NVIDIA GeForce RTX 3050 Laptop GPU"). It is the
	// ONLY field that can be correlated with daemon/gpu's nvidia-smi view.
	Name string
	// TotalMiB/FreeMiB are as ggml reports them. NOTE these are Vulkan's numbers and
	// deliberately are NOT reconciled with nvidia-smi's — see the file comment.
	TotalMiB uint64
	FreeMiB  uint64
}

func (d GGMLDevice) String() string {
	return fmt.Sprintf("%s (%s, %d MiB free / %d MiB)", d.ID, d.Name, d.FreeMiB, d.TotalMiB)
}

// listDevicesTimeout bounds a llama-server that hangs enumerating a wedged driver.
const listDevicesTimeout = 20 * time.Second

// deviceRe parses one row of `llama-server --list-devices`:
//
//	Vulkan1: NVIDIA GeForce RTX 3050 Laptop GPU (3964 MiB, 3369 MiB free)
//
// The name itself contains parentheses ("Intel(R) Iris(R) Xe Graphics"), so the
// name group is greedy and the memory group is anchored to end-of-line: the last
// "(N MiB, M MiB free)" wins and everything before it is the name.
var deviceRe = regexp.MustCompile(`^\s*([A-Za-z0-9_.:-]+):\s+(.+)\s+\((\d+)\s*MiB,\s*(\d+)\s*MiB free\)\s*$`)

// parseListDevices extracts the device table from --list-devices output.
//
// A row that does not parse is SKIPPED, never defaulted — same discipline as
// daemon/gpu.parseNvidiaSMI. Inventing a device ID here would send -d a name that
// does not exist; upstream would refuse to start and the node would look broken for
// a reason no log explains.
func parseListDevices(out string) []GGMLDevice {
	var devs []GGMLDevice
	for _, line := range strings.Split(out, "\n") {
		m := deviceRe.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		total, err1 := strconv.ParseUint(m[3], 10, 64)
		free, err2 := strconv.ParseUint(m[4], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		devs = append(devs, GGMLDevice{
			ID:       m[1],
			Name:     strings.TrimSpace(m[2]),
			TotalMiB: total,
			FreeMiB:  free,
		})
	}
	return devs
}

// ListDevices asks llama-server which accelerators ggml can actually see.
//
// It returns the devices -d will accept, in ggml's own order. The list contains
// accelerators only: upstream omits the CPU device from --list-devices.
func ListDevices(ctx context.Context, bins Binaries) ([]GGMLDevice, error) {
	if bins.Server == "" {
		return nil, fmt.Errorf("llama: cannot enumerate devices: no llama-server located")
	}
	ctx, cancel := context.WithTimeout(ctx, listDevicesTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bins.Server, "--list-devices")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Backend-load chatter goes to stderr; the table goes to stdout. Tolerate a
	// non-zero exit as long as the table parsed — the output is the contract.
	runErr := cmd.Run()

	devs := parseListDevices(stdout.String())
	if len(devs) == 0 {
		if runErr != nil {
			return nil, fmt.Errorf("llama: %s --list-devices failed: %w (stderr: %s)",
				bins.Server, runErr, truncate(strings.TrimSpace(stderr.String()), 200))
		}
		// No accelerators is a legitimate answer (CPU-only box), not an error.
		return nil, nil
	}
	return devs, nil
}

// bareIntRe catches the exact mistake this file exists to prevent.
var bareIntRe = regexp.MustCompile(`^\d+$`)

// SelectDevice resolves an operator's device choice against what ggml can see.
//
// want is a ggml device ID ("Vulkan1"), optionally a comma-separated list to lend
// several. Matching is case-insensitive. An unknown ID is REFUSED with the real
// device table rather than passed through, and a bare integer ("1") is refused with
// a pointed explanation: integers are ambiguous between nvidia-smi's ordering and
// Vulkan's, and on the common iGPU+dGPU laptop the two disagree.
func SelectDevice(devs []GGMLDevice, want string) (string, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return "", fmt.Errorf("llama: SelectDevice: empty device")
	}
	var out []string
	for _, part := range strings.Split(want, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if bareIntRe.MatchString(part) {
			return "", fmt.Errorf(
				"llama: device %q is a bare index, which is ambiguous and will lend the wrong card.\n"+
					"nvidia-smi and Vulkan number devices DIFFERENTLY: on an iGPU+dGPU laptop nvidia-smi's "+
					"index 0 is the discrete card while Vulkan's device 0 is the integrated one.\n"+
					"Name the ggml device explicitly instead.%s", part, availableSuffix(devs))
		}
		match, ok := findDevice(devs, part)
		if !ok {
			return "", fmt.Errorf("llama: no ggml device named %q on this node.%s", part, availableSuffix(devs))
		}
		out = append(out, match.ID)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("llama: SelectDevice: no device named in %q", want)
	}
	return strings.Join(out, ","), nil
}

func findDevice(devs []GGMLDevice, id string) (GGMLDevice, bool) {
	for _, d := range devs {
		if strings.EqualFold(d.ID, id) {
			return d, true
		}
	}
	return GGMLDevice{}, false
}

func availableSuffix(devs []GGMLDevice) string {
	if len(devs) == 0 {
		return " This node reports no ggml accelerators at all."
	}
	var b strings.Builder
	b.WriteString(" Available: ")
	for i, d := range devs {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(d.String())
	}
	return b.String()
}

// DefaultDevice picks the ONE device this node lends when the operator named none.
//
// # Why a single device, and why not upstream's default
//
// Omitting -d does NOT mean "pick the best GPU" — measured at b10021, it means
// SERVE EVERY DEVICE, and llama.cpp's scheduler then splits the model and KV cache
// across all of them. On this box that silently conscripted the Intel iGPU into
// every offload job: with -d Vulkan1 the RTX 3050 held 646 MiB of a 400k-token KV
// cache; with no -d it held only 452 MiB because the iGPU had taken the rest.
//
// That default breaks an invariant the rest of the daemon depends on. daemon/gpu's
// VRAM.Free() is deliberately the SINGLE BEST DEVICE and explicitly not a sum
// across devices, because — in that file's own words — "a task placed on this node
// runs on one device and can only use that device's memory". Telemetry advertises
// one card's free VRAM and daemon/scheduler.PlaceGPU ranks nodes on it. A worker
// that then spreads the job over two devices with a different pool makes that
// advertised number a fiction, and drags a slow uma iGPU into a job the scheduler
// sized for a dGPU.
//
// So the default resolves to the SAME device telemetry is advertising, matched by
// name. Multi-device is still available — it just has to be asked for explicitly
// (Device: "Vulkan0,Vulkan1"), because on a node with two real dGPUs that is a
// sensible thing to want and the operator is the one who knows it.
//
// It returns the chosen device and a human-readable reason for the log. If it
// cannot choose honestly it returns ok=false, and the caller must NOT invent one.
func DefaultDevice(devs []GGMLDevice, snap gpu.VRAM) (GGMLDevice, string, bool) {
	if len(devs) == 0 {
		return GGMLDevice{}, "", false
	}

	// Preferred: the device telemetry advertises, so what the scheduler promised is
	// what the worker delivers. Correlate by NAME — never by index.
	if snap.Known() {
		if best, ok := bestVRAMDevice(snap); ok {
			for _, d := range devs {
				if strings.EqualFold(strings.TrimSpace(d.Name), strings.TrimSpace(best.Name)) {
					return d, fmt.Sprintf(
						"it is the device telemetry advertises (%s reports %d MiB free on %q)",
						snap.Source, best.FreeBytes/(1024*1024), best.Name), true
				}
			}
		}
		// Telemetry sees a card ggml cannot, or vice versa. Say so plainly rather
		// than quietly falling back — this is the state where a wrong guess lends the
		// wrong card.
		return pickMostFree(devs, fmt.Sprintf(
			"telemetry (%s) and ggml disagree about this machine's devices, so the advertised "+
				"card could not be matched by name; falling back to ggml's own view", snap.Source))
	}

	// No measurable NVIDIA GPU: telemetry advertises unknown VRAM, so there is no
	// advertised card to be consistent with. ggml's own view is all we have.
	return pickMostFree(devs, "telemetry could not measure this machine's VRAM, so ggml's own view is used")
}

func pickMostFree(devs []GGMLDevice, why string) (GGMLDevice, string, bool) {
	if len(devs) == 0 {
		return GGMLDevice{}, "", false
	}
	best := devs[0]
	for _, d := range devs[1:] {
		if d.FreeMiB > best.FreeMiB {
			best = d
		}
	}
	return best, fmt.Sprintf("it has the most free memory of the devices ggml can see (%s)", why), true
}

// bestVRAMDevice mirrors daemon/gpu's own reduction (most free wins) so the two
// cannot drift: VRAM.Free() reports this device's memory, so this is the device the
// worker must use.
func bestVRAMDevice(v gpu.VRAM) (gpu.Device, bool) {
	if !v.Known() {
		return gpu.Device{}, false
	}
	best := v.Devices[0]
	for _, d := range v.Devices[1:] {
		if d.FreeBytes > best.FreeBytes {
			best = d
		}
	}
	return best, true
}
