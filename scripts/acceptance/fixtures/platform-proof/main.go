package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	if proof, err := os.ReadFile("/platform-proof"); err == nil {
		fmt.Print(string(proof))
		return
	}
	if err := os.WriteFile("/platform-proof", []byte(runtime.GOARCH+"\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
