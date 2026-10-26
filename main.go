// Netonix u. a. liefern Zertifikate mit negativer Seriennummer (Go lehnt die sonst ab)
//
//go:debug x509negativeserial=1
package main

import (
	"fmt"
	"os"
	_ "time/tzdata"
)

var version = "1.7.1"

func main() {
	mode := defaultMode
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "server", "agent":
			mode = args[0]
			args = args[1:]
		case "service":
			runService(args[1:])
			return
		case "version", "-v", "--version":
			fmt.Println("PingWacht", version)
			return
		}
	}
	switch mode {
	case "server":
		runServer(args)
	default:
		runAgent(args)
	}
}
