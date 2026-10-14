// coopr-sequoia-signing is an optional native signing backend, never a container runtime dependency.
package main

import (
	"coopr/internal/buildah"
	_ "coopr/internal/transfer"
	"fmt"
	"os"
)

func main() {
	if buildah.InitReexec() {
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--protocol-version" && buildah.NativeSequoia {
		fmt.Println(buildah.SequoiaHelperProtocol)
		return
	}
	fmt.Fprintln(os.Stderr, "coopr-sequoia-signing requires the Sequoia build tag and Coopr's private worker protocol")
	os.Exit(2)
}
