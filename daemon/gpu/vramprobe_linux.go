//go:build linux

package gpu

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// sources on Linux: nvidia-smi first (same tool, same flags, same output format as
// the Windows path — it ships in the NVIDIA driver on both), then the amdgpu
// kernel driver's sysfs counters for AMD cards.
func sources() []source {
	return []source{
		{name: nvidiaSMISourceName, probe: probeNvidiaSMI},
		{name: amdgpuSysfsSourceName, probe: probeAMDGPUSysfs},
	}
}

// amdgpuSysfsSourceName reads the amdgpu driver's own VRAM accounting out of
// sysfs. These files are exported directly by the kernel driver in bytes, need no
// userspace library, no subprocess and no cgo, and are the same counters
// radeontop/rocm-smi surface.
//
//	/sys/class/drm/card0/device/mem_info_vram_total   total device VRAM, bytes
//	/sys/class/drm/card0/device/mem_info_vram_used    currently allocated, bytes
//
// UNVERIFIED ON HARDWARE: the GPU lane had no AMD box available. The parser is
// unit-tested against a synthetic sysfs tree (see vramprobe_linux_test.go), but the
// real-driver path has not been exercised. It fails closed — any missing or
// unparsable file drops the card, and a card-less result reports unknown rather
// than a guess.
const amdgpuSysfsSourceName = "amdgpu-sysfs"

// sysfsDRMRoot is the DRM class root, indirected so tests can point at a fixture.
var sysfsDRMRoot = "/sys/class/drm"

// probeAMDGPUSysfs enumerates /sys/class/drm/card* and reads each amdgpu card's
// VRAM counters.
//
// Unlike nvidia-smi, sysfs exposes `used` rather than `free`, so free is derived
// as total-used. That subtraction is sound here in a way it is NOT for nvidia-smi:
// these two counters come from the same allocator accounting for the same pool, so
// the difference is the allocator's own free figure rather than an unrelated
// reserve. A card whose used > total (should be impossible) is dropped rather than
// underflowed.
func probeAMDGPUSysfs() ([]Device, error) {
	entries, err := filepath.Glob(filepath.Join(sysfsDRMRoot, "card[0-9]*"))
	if err != nil {
		return nil, fmt.Errorf("amdgpu-sysfs: glob: %w", err)
	}
	sort.Strings(entries)

	var devs []Device
	for _, dir := range entries {
		// Skip connector nodes like card0-DP-1: only the card itself has device/.
		base := filepath.Base(dir)
		if strings.Contains(base, "-") {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimPrefix(base, "card"))
		if err != nil {
			continue
		}
		total, err := readUintFile(filepath.Join(dir, "device", "mem_info_vram_total"))
		if err != nil {
			continue // not an amdgpu card (nouveau/i915 export no such file)
		}
		used, err := readUintFile(filepath.Join(dir, "device", "mem_info_vram_used"))
		if err != nil {
			continue
		}
		if total == 0 || used > total {
			continue // nonsense; report nothing rather than something wrong
		}
		devs = append(devs, Device{
			Index:      idx,
			Name:       amdgpuName(dir, base),
			TotalBytes: total,
			FreeBytes:  total - used,
		})
	}
	if len(devs) == 0 {
		return nil, errors.New("amdgpu-sysfs: no amdgpu cards with VRAM counters")
	}
	return devs, nil
}

// amdgpuName labels the card. sysfs has no marketing name, so we use the PCI
// device id when readable and fall back to the card node — an honest identifier
// rather than an invented product name.
func amdgpuName(dir, base string) string {
	if b, err := os.ReadFile(filepath.Join(dir, "device", "device")); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return fmt.Sprintf("amdgpu %s (%s)", base, id)
		}
	}
	return "amdgpu " + base
}

func readUintFile(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}
