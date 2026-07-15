package gpu

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// nvidiaSMISource reads per-device VRAM from `nvidia-smi`, the query tool that
// ships inside every NVIDIA driver package on Windows and Linux (on this repo's
// Windows test box it resolves to C:\Windows\System32\nvidia-smi.exe; on Linux it
// is /usr/bin/nvidia-smi). It needs no SDK, no headers, no cgo and no separate
// install, which is what makes it viable under the CGO_ENABLED=0 release matrix.
//
// It is also, by the GPU lane's own brief, the ground truth: `memory.free` here is
// the number a human runs nvidia-smi to read. Sourcing the scheduler from the same
// place a human checks means the two can never silently disagree.
//
// Trade-off accepted: a ~130ms process spawn per call. Monitor's TTL cache makes
// that a non-issue at telemetry's 2 Hz.
const nvidiaSMISourceName = "nvidia-smi"

// nvidiaSMIQuery keeps `free` as its own queried field rather than deriving it
// from total-used. On the RTX 3050 the driver reports total=4096, used=104,
// free=3861 MiB: total-used would be 3992, overstating free by 131 MiB of memory
// the driver has reserved and will not hand out. Derived arithmetic here would be
// a fabrication of exactly the kind that OOMs a --tensor-split.
var nvidiaSMIQuery = []string{
	"--query-gpu=index,name,memory.total,memory.free",
	"--format=csv,noheader,nounits",
}

// nvidiaSMITimeout bounds a wedged nvidia-smi (it can hang on a GPU in a bad
// state, or one being reset) so it can never stall a telemetry sample.
const nvidiaSMITimeout = 4 * time.Second

// lookPath / runCommand are indirected for tests.
var (
	lookPath   = exec.LookPath
	runCommand = func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, bin, args...).Output()
	}
)

// probeNvidiaSMI returns one Device per NVIDIA GPU, or an error if nvidia-smi is
// absent (no NVIDIA driver — the common case on most machines) or unusable.
func probeNvidiaSMI() ([]Device, error) {
	bin, err := lookPath("nvidia-smi")
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi not on PATH (no NVIDIA driver?): %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), nvidiaSMITimeout)
	defer cancel()

	out, err := runCommand(ctx, bin, nvidiaSMIQuery...)
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	return parseNvidiaSMI(string(out))
}

// parseNvidiaSMI parses `--format=csv,noheader,nounits` rows of
// `index,name,memory.total,memory.free`, e.g.
//
//	0, NVIDIA GeForce RTX 3050 Laptop GPU, 4096, 3861
//
// Memory columns are MiB under `nounits`.
//
// A row whose numbers do not parse is SKIPPED, never defaulted. nvidia-smi prints
// "[N/A]" or "[Not Supported]" for memory on some configurations (notably vGPU
// guests and some MIG setups); turning that into a zero — or worse, a plausible
// number — is precisely the failure this package refuses. If no row survives, the
// caller reports unknown.
func parseNvidiaSMI(out string) ([]Device, error) {
	var devs []Device
	var skipped []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cols := strings.Split(line, ",")
		if len(cols) != 4 {
			skipped = append(skipped, line)
			continue
		}
		idx, err1 := strconv.Atoi(strings.TrimSpace(cols[0]))
		totalMiB, err2 := strconv.ParseUint(strings.TrimSpace(cols[2]), 10, 64)
		freeMiB, err3 := strconv.ParseUint(strings.TrimSpace(cols[3]), 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			skipped = append(skipped, line)
			continue
		}
		if totalMiB == 0 {
			// A device reporting zero total memory has told us nothing usable.
			skipped = append(skipped, line)
			continue
		}
		devs = append(devs, Device{
			Index:      idx,
			Name:       strings.TrimSpace(cols[1]),
			TotalBytes: totalMiB * 1024 * 1024,
			FreeBytes:  freeMiB * 1024 * 1024,
		})
	}
	if len(devs) == 0 {
		if len(skipped) > 0 {
			return nil, fmt.Errorf("nvidia-smi: no parsable device rows (e.g. %q)", skipped[0])
		}
		return nil, errors.New("nvidia-smi: no devices reported")
	}
	return devs, nil
}
