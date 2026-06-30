package main

import (
	"fmt"
	"log"
	"net/rpc"
	"os"

	"github.com/hash066/cerberus/daemon/auth"
)

type StatusRequest struct{ Token string }
type StatusResponse struct {
	Version string
	State   string
	Subject string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: cerberus <command>")
		fmt.Println("Commands: status, run")
		fmt.Println("Auth: reads $CERBERUS_TOKEN or the operator token file written by cerberusd.")
		os.Exit(1)
	}

	token := auth.LoadToken()
	if token == "" {
		log.Fatalf("no capability token found: set $CERBERUS_TOKEN or start cerberusd (writes %s)", auth.OperatorTokenPath())
	}

	cmd := os.Args[1]

	client, err := rpc.Dial("tcp", "127.0.0.1:9092")
	if err != nil {
		log.Fatalf("Failed to connect to cerberusd: %v", err)
	}
	defer client.Close()

	switch cmd {
	case "status":
		req := StatusRequest{Token: token}
		var resp StatusResponse
		if err := client.Call("DaemonRPC.Status", &req, &resp); err != nil {
			log.Fatalf("RPC error: %v", err)
		}
		fmt.Printf("Cerberus Daemon Status\n")
		fmt.Printf("Version: %s\n", resp.Version)
		fmt.Printf("State:   %s\n", resp.State)
		fmt.Printf("Auth:    authenticated as %q\n", resp.Subject)
	case "run":
		fmt.Println("Run command stub. Will dispatch to gateway/scheduler in the future.")
	default:
		fmt.Printf("Unknown command: %s\n", cmd)
		os.Exit(1)
	}
}
