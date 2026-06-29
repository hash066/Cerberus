// Command cerberus is the Cerberus CLI (vertical 10).
// v0.1 skeleton: version/status/run subcommands. Lane C grows this onto the
// gRPC-over-UDS IPC contract (ARCHITECTURE.md §3.8) talking to cerberusd.
package main

import (
	"fmt"
	"os"

	contract "github.com/hash066/cerberus/contract/go"
)

func usage() {
	fmt.Print(`cerberus — Cerberus CLI (v0.1 skeleton)

usage:
  cerberus version              print contract version
  cerberus status               show local node status (stub)
  cerberus run <wasm> --on <p>  run a capability-gated WASM task on a peer (integration)
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Printf("cerberus %s\n", contract.ContractVersion)
	case "status":
		fmt.Println("node: local  state: skeleton  peers: 0  (IPC to cerberusd wired during integration)")
	case "run":
		fmt.Println("run: requires a live cerberusd + mesh (integration milestone) — see test/e2e for the in-process demo")
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}
