package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	if len(os.Args) == 3 {
		marker, err := os.ReadFile(os.Args[1])
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(os.Args[2], []byte(runtime.GOARCH+":"+string(marker)), 0o644); err != nil {
			panic(err)
		}
		return
	}
	if len(os.Args) == 2 {
		proof, err := os.ReadFile(os.Args[1])
		if err != nil {
			panic(err)
		}
		fmt.Print(string(proof))
		return
	}
	fmt.Print(runtime.GOARCH)
}
