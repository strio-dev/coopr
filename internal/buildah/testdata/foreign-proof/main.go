package main

import (
	"os"
	"runtime"
)

func main() {
	if err := os.WriteFile("/executed", []byte(runtime.GOARCH+"\n"), 0o644); err != nil {
		panic(err)
	}
}
