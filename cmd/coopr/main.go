package main

import (
	"context"
	"os"

	"github.com/sirupsen/logrus"
	"github.com/strio-dev/coopr"
)

func main() {
	logrus.SetLevel(logrus.WarnLevel)
	if coopr.InitReexec() {
		return
	}
	os.Exit(runContextWithStorageNamespace(context.Background(), os.Args[1:], os.Stdout, os.Stderr, prepareStorageNamespace))
}
