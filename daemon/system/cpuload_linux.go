//go:build linux

package system

import (
	"os"
	"strconv"
	"strings"
)

// readCPUTimes reads Linux' aggregate CPU jiffies from the first ("cpu ") line
// of /proc/stat. Fields are: user nice system idle iowait irq softirq steal
// guest guest_nice. total is the sum of all fields; idle counts idle+iowait
// (both are "not doing useful work" from a placement standpoint). The sampler
// only differences successive reads, so jiffy units cancel out.
func readCPUTimes() (idle, total uint64, ok bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		var sum, idleV uint64
		for i, f := range fields {
			v, perr := strconv.ParseUint(f, 10, 64)
			if perr != nil {
				continue
			}
			sum += v
			if i == 3 || i == 4 { // idle, iowait
				idleV += v
			}
		}
		if sum == 0 {
			return 0, 0, false
		}
		return idleV, sum, true
	}
	return 0, 0, false
}
