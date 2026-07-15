//go:build linux

package gpu

import (
	"os"
	"path/filepath"
	"testing"
)

// writeCard lays down a synthetic /sys/class/drm/cardN/device tree.
func writeCard(t *testing.T, root, card string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, card, "device")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func withFakeSysfs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	orig := sysfsDRMRoot
	sysfsDRMRoot = root
	t.Cleanup(func() { sysfsDRMRoot = orig })
	return root
}

// Real amdgpu counters are bytes. These are an RX 6700 XT's 12 GiB with ~1 GiB
// allocated — the shape the driver actually exports.
func TestProbeAMDGPUSysfs(t *testing.T) {
	root := withFakeSysfs(t)
	writeCard(t, root, "card0", map[string]string{
		"mem_info_vram_total": "12884901888\n",
		"mem_info_vram_used":  "1073741824\n",
		"device":              "0x73df\n",
	})

	devs, err := probeAMDGPUSysfs()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("want 1 device, got %d", len(devs))
	}
	d := devs[0]
	if d.TotalBytes != 12884901888 {
		t.Errorf("TotalBytes = %d, want 12884901888", d.TotalBytes)
	}
	if want := uint64(12884901888 - 1073741824); d.FreeBytes != want {
		t.Errorf("FreeBytes = %d, want %d (total-used)", d.FreeBytes, want)
	}
	if d.Index != 0 {
		t.Errorf("Index = %d, want 0", d.Index)
	}
}

func TestProbeAMDGPUSysfsMultiCard(t *testing.T) {
	root := withFakeSysfs(t)
	writeCard(t, root, "card0", map[string]string{
		"mem_info_vram_total": "8589934592\n",
		"mem_info_vram_used":  "0\n",
	})
	writeCard(t, root, "card1", map[string]string{
		"mem_info_vram_total": "17179869184\n",
		"mem_info_vram_used":  "1024\n",
	})
	devs, err := probeAMDGPUSysfs()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(devs) != 2 {
		t.Fatalf("want 2 devices, got %+v", devs)
	}
	if devs[0].Index != 0 || devs[1].Index != 1 {
		t.Errorf("indices = %d,%d", devs[0].Index, devs[1].Index)
	}
}

// A non-amdgpu card (i915/nouveau export no mem_info_* files) must be skipped,
// not defaulted to zero.
func TestProbeAMDGPUSysfsSkipsNonAMD(t *testing.T) {
	root := withFakeSysfs(t)
	writeCard(t, root, "card0", map[string]string{"device": "0x9a49\n"}) // Intel, no counters
	if devs, err := probeAMDGPUSysfs(); err == nil {
		t.Fatalf("want error (no amdgpu cards), got %+v", devs)
	}
}

// Connector nodes (card0-DP-1) sit alongside cards in /sys/class/drm and must not
// be mistaken for devices.
func TestProbeAMDGPUSysfsIgnoresConnectorNodes(t *testing.T) {
	root := withFakeSysfs(t)
	writeCard(t, root, "card0", map[string]string{
		"mem_info_vram_total": "8589934592\n",
		"mem_info_vram_used":  "0\n",
	})
	writeCard(t, root, "card0-DP-1", map[string]string{
		"mem_info_vram_total": "999\n",
		"mem_info_vram_used":  "0\n",
	})
	devs, err := probeAMDGPUSysfs()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(devs) != 1 || devs[0].TotalBytes != 8589934592 {
		t.Fatalf("connector node leaked into results: %+v", devs)
	}
}

// Garbage must never become a number.
func TestProbeAMDGPUSysfsRefusesNonsense(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"used > total": {"mem_info_vram_total": "1024\n", "mem_info_vram_used": "2048\n"},
		"zero total":   {"mem_info_vram_total": "0\n", "mem_info_vram_used": "0\n"},
		"unparsable":   {"mem_info_vram_total": "banana\n", "mem_info_vram_used": "0\n"},
		"missing used": {"mem_info_vram_total": "8589934592\n"},
	} {
		t.Run(name, func(t *testing.T) {
			root := withFakeSysfs(t)
			writeCard(t, root, "card0", files)
			if devs, err := probeAMDGPUSysfs(); err == nil {
				t.Fatalf("want error, got %+v", devs)
			}
		})
	}
}

func TestProbeAMDGPUSysfsMissingRoot(t *testing.T) {
	orig := sysfsDRMRoot
	sysfsDRMRoot = "/nonexistent/drm/root"
	t.Cleanup(func() { sysfsDRMRoot = orig })
	if _, err := probeAMDGPUSysfs(); err == nil {
		t.Fatal("want error when sysfs root is absent")
	}
}
