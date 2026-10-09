package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	data, err := os.ReadFile("/marker")
	if err != nil {
		panic(err)
	}
	fmt.Print("loaded:", strings.TrimSpace(string(data)))
}
