// Command mmp is the IPTV multiplexer / failover proxy server.
package main

import (
	"context"
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "mmp: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	fmt.Fprintf(os.Stderr, "mmp %s starting (scaffold)\n", version)
	return nil
}
