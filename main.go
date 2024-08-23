//go:build !test

// Command pkgtrim is a helper tool to maintain the list of installed packages in linux.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := Pkgtrim(os.Stdout, os.DirFS("/"), os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v.\n", err)
		os.Exit(1)
	}
}
