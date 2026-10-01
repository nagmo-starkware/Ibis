package main

import (
	"fmt"
	"os"

	"github.com/b-j-roberts/ibis/internal/cli"
	"github.com/b-j-roberts/ibis/internal/provider"
)

// Set by goreleaser ldflags at build time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	cli.SetVersion(version, commit, date)
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, errMessage(err))
		os.Exit(1)
	}
}

// errMessage is the final error text; RPC URLs carry the API key.
func errMessage(err error) string { return provider.RedactURLs(err.Error()) }
