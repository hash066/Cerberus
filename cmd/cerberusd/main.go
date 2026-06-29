// Command cerberusd is the headless Cerberus daemon (vertical 10).
// v0.1 skeleton: boots the capability kernel and reports status. Lane C/A grow
// this into the supervised daemon hosting mesh, scheduler, 9P, runtime, etc.
package main

import (
	"flag"
	"fmt"
	"os"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/ffi"
)

func main() {
	profile := flag.String("profile", "open_mesh", "open_mesh | sealed")
	flag.Parse()

	k := ffi.NewKernel()
	// Smoke: mint a VRAM capability to prove the kernel is live.
	h, err := k.Mint(
		contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/dev/vram/local/0"},
		[]contract.Right{contract.RightRead, contract.RightAlloc},
		nil,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cerberusd: kernel init failed:", err)
		os.Exit(1)
	}

	fmt.Printf("cerberusd v%s  profile=%s  kernel=%s  sample-cap=%d\n",
		contract.ContractVersion, *profile, ffi.Backend(), h)
	fmt.Println("cerberusd: control plane up (v0.1 skeleton). Subsystems wired during integration.")
}
