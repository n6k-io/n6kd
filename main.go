package main

import (
	"os"

	"github.com/n6k-io/n6kd/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}
