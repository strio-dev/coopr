package main

import (
	"context"
	"fmt"
	"path/filepath"

	"coopr/internal/buildah"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type storageSelection struct {
	store buildah.StoreOptions
}

type storageSelectionKey struct{}

const nativeStorageAnnotation = "coopr.native-storage"

func addGlobalFlags(root *cobra.Command, prepareNamespace func() error) {
	f := root.PersistentFlags()
	var graphRoot, runRoot, driver, imageStoreDir string
	var storageOptions []string
	var logLevel string
	var transient bool
	f.StringVar(&logLevel, "log-level", "warn", "diagnostic log level: trace, debug, info, warn, error, fatal, panic")
	f.StringVar(&graphRoot, "root", "", "containers/storage graph root (default: effective storage.conf)")
	f.StringVar(&runRoot, "runroot", "", "containers/storage runtime root")
	f.StringVar(&driver, "storage-driver", "", "containers/storage driver")
	f.StringArrayVar(&storageOptions, "storage-opt", nil, "containers/storage driver option (repeatable)")
	f.StringVar(&imageStoreDir, "imagestore", "", "separate native image storage directory")
	f.BoolVar(&transient, "transient-store", false, "keep transient container metadata in the runtime root")
	addGlobalRunFlags(f)
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		level, err := logrus.ParseLevel(logLevel)
		if err != nil {
			return fmt.Errorf("parse --log-level: %w", err)
		}
		logrus.SetLevel(level)
		logrus.SetOutput(cmd.ErrOrStderr())
		if cmd.Annotations[nativeStorageAnnotation] != "true" {
			return nil
		}
		if prepareNamespace != nil {
			if err := prepareNamespace(); err != nil {
				return err
			}
		}
		store, err := defaultCommandStorage()
		if err != nil {
			return err
		}
		if graphRoot != "" {
			store.GraphRoot, err = filepath.Abs(graphRoot)
			if err != nil {
				return err
			}
		}
		if runRoot != "" {
			store.RunRoot, err = filepath.Abs(runRoot)
			if err != nil {
				return err
			}
		}
		if driver != "" {
			store.GraphDriverName = driver
			store.GraphDriverOptions = nil
		}
		if f.Changed("storage-opt") {
			store.GraphDriverOptions = append([]string(nil), storageOptions...)
		}
		if imageStoreDir != "" {
			store.ImageStore, err = filepath.Abs(imageStoreDir)
			if err != nil {
				return err
			}
		}
		if f.Changed("transient-store") {
			store.TransientStore = transient
		}
		store, err = buildah.NormalizeStoreOptions(store)
		if err != nil {
			return err
		}
		cmd.SetContext(context.WithValue(cmd.Context(), storageSelectionKey{}, storageSelection{store}))
		return nil
	}
}

// Standalone constructors use the same declarations as root persistent flags.
func addGlobalRunFlags(set *pflag.FlagSet) {
	set.String("cgroup-manager", "", "cgroup manager: systemd or cgroupfs")
	set.StringArray("module", nil, "containers.conf module (repeatable)")
	set.StringArray("cdi-spec-dir", nil, "CDI specification directory (repeatable)")
	set.String("network-config-dir", "", "native network configuration directory")
}

func addSignaturePolicyFlag(set *pflag.FlagSet) {
	set.String("signature-policy", "", "containers/image signature policy file")
	_ = set.MarkHidden("signature-policy")
}

func commandStorage(cmd *cobra.Command) (buildah.StoreOptions, error) {
	if selected, ok := cmd.Context().Value(storageSelectionKey{}).(storageSelection); ok {
		return selected.store, nil
	}
	return defaultCommandStorage()
}
func defaultCommandStorage() (buildah.StoreOptions, error) { return buildah.DefaultStoreOptions() }
