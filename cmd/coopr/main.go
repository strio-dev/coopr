package main

import (
	"os"

	"coopr/internal/buildah"
	"github.com/sirupsen/logrus"
)

func main() {
	logrus.SetLevel(logrus.WarnLevel)
	if buildah.InitReexec() {
		return
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
