package main

import (
	"fmt"
	"log"
	"net/rpc"
	"os"
)

type StatusRequest struct{}
type StatusResponse struct {
	Version string
	State   string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: cerberus <command>")
		fmt.Println("Commands: status, run")
		os.Exit(1)
	}

	cmd := os.Args[1]

	client, err := rpc.Dial("tcp", "127.0.0.1:9092")
	if err != nil {
		log.Fatalf("Failed to connect to cerberusd: %v", err)
	}
	defer client.Close()

	switch cmd {
	case "status":
		var req StatusRequest
		var resp StatusResponse
		err = client.Call("DaemonRPC.Status", &req, &resp)
		if err != nil {
			log.Fatalf("RPC error: %v", err)
		}
		fmt.Printf("Cerberus Daemon Status\n")
		fmt.Printf("Version: %s\n", resp.Version)
		fmt.Printf("State:   %s\n", resp.State)
	case "run":
		fmt.Println("Run command stub. Will dispatch to gateway/scheduler in the future.")
	default:
		fmt.Printf("Unknown command: %s\n", cmd)
		os.Exit(1)
	}
}
