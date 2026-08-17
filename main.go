package main

import (
	"os"

	"github.com/n6k-io/duckdb-driver/packages/go/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}
