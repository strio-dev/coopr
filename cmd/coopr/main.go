package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/strio-dev/coopr"
)

func main() {
	logrus.SetLevel(logrus.WarnLevel)
	if coopr.InitReexec() {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runContextWithStorageNamespace(ctx, os.Args[1:], os.Stdout, os.Stderr, prepareStorageNamespace)
	stop()
	os.Exit(code)
}
