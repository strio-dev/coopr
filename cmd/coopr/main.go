package main

import (
	"context"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/strio-dev/coopr"
)

func main() {
	logrus.SetLevel(logrus.WarnLevel)
	// Match Buildah's numeric diagnostic-level convention for private reexecs.
	if level, err := strconv.Atoi(os.Getenv("LOGLEVEL")); err == nil && level >= int(logrus.PanicLevel) && level <= int(logrus.TraceLevel) {
		logrus.SetLevel(logrus.Level(level))
	}
	_ = os.Unsetenv("LOGLEVEL")
	if coopr.InitReexec() {
		return
	}
	// Ordinary CLI defaults and explicit flags remain independent of helper state.
	logrus.SetLevel(logrus.WarnLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runContextWithStorageNamespace(ctx, os.Args[1:], os.Stdout, os.Stderr, prepareStorageNamespace)
	stop()
	os.Exit(code)
}
