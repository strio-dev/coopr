package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"coopr/internal/buildah"
	"coopr/internal/localstore"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type storageSelection struct {
	store   buildah.StoreOptions
	catalog string
}

type storageSelectionKey struct{}

func addGlobalFlags(root *cobra.Command) {
	f := root.PersistentFlags()
	var graphRoot, runRoot, driver, imageStoreDir, imageStoreMode string
	var storageOptions []string
	var logLevel string
	var transient bool
	f.StringVar(&logLevel, "log-level", "warn", "diagnostic log level: trace, debug, info, warn, error, fatal, panic")
	f.StringVar(&graphRoot, "root", "", "containers/storage graph root (default: Coopr's image graph)")
	f.StringVar(&runRoot, "runroot", "", "containers/storage runtime root")
	f.StringVar(&driver, "storage-driver", "", "containers/storage driver")
	f.StringArrayVar(&storageOptions, "storage-opt", nil, "containers/storage driver option (repeatable)")
	f.StringVar(&imageStoreDir, "imagestore", "", "separate native image storage directory")
	f.StringVar(&imageStoreMode, "image-store", "", "default image store: coopr or podman (overrides config.toml)")
	f.BoolVar(&transient, "transient-store", false, "keep transient container metadata in the runtime root")
	addGlobalRunFlags(f)
	addSignaturePolicyFlag(f)
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		level, err := logrus.ParseLevel(logLevel)
		if err != nil {
			return fmt.Errorf("parse --log-level: %w", err)
		}
		logrus.SetLevel(level)
		logrus.SetOutput(cmd.ErrOrStderr())
		config, err := loadCooprConfig()
		if err != nil {
			return err
		}
		mode := config.ImageStore
		if f.Changed("image-store") {
			mode = strings.ToLower(imageStoreMode)
			if err := validateImageStore(mode); err != nil {
				return err
			}
		}
		store, catalog, err := defaultCommandStorage(mode)
		if err != nil {
			return err
		}
		customStorage := f.Changed("root") || f.Changed("runroot") || f.Changed("storage-driver") || f.Changed("storage-opt") || f.Changed("imagestore") || f.Changed("transient-store")
		if customStorage {
			// Arbitrary stores may be shared with another containers/storage
			// client. Maintenance must retain unowned image records there.
			store.Shared = true
		}
		if graphRoot != "" {
			store.GraphRoot, err = filepath.Abs(graphRoot)
			if err != nil {
				return err
			}
			if mode == imageStoreCoopr {
				catalog = filepath.Join(store.GraphRoot, "coopr")
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
		if mode == imageStorePodman || customStorage {
			catalog, err = selectedStoreCatalog(store)
			if err != nil {
				return err
			}
		}
		cmd.SetContext(context.WithValue(cmd.Context(), storageSelectionKey{}, storageSelection{store, catalog}))
		return nil
	}
}

func selectedStoreCatalog(store buildah.StoreOptions) (string, error) {
	base, err := localstore.DefaultImageDir()
	if err != nil {
		return "", err
	}
	identity, err := buildah.StoreIdentity(store)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "catalogs", identity), nil
}

// Standalone constructors use the same declarations as root persistent flags.
func addGlobalRunFlags(set *pflag.FlagSet) {
	set.String("cgroup-manager", "", "cgroup manager: systemd or cgroupfs")
	set.StringArray("module", nil, "containers.conf module (repeatable)")
	set.StringArray("cdi-spec-dir", nil, "CDI specification directory (repeatable)")
	set.String("network-config-dir", "", "native network configuration directory")
	set.String("network-cmd-path", "", "slirp4netns helper executable")
}

func addSignaturePolicyFlag(set *pflag.FlagSet) {
	set.String("signature-policy", "", "containers/image signature policy file")
}

func commandStorage(cmd *cobra.Command) (buildah.StoreOptions, string, error) {
	if selected, ok := cmd.Context().Value(storageSelectionKey{}).(storageSelection); ok {
		return selected.store, selected.catalog, nil
	}
	config, err := loadCooprConfig()
	if err != nil {
		return buildah.StoreOptions{}, "", err
	}
	return defaultCommandStorage(config.ImageStore)
}

func defaultCommandStorage(mode string) (buildah.StoreOptions, string, error) {
	var store buildah.StoreOptions
	var err error
	if mode == imageStorePodman {
		store, err = buildah.PodmanStoreOptions()
	} else {
		store, err = buildah.DefaultStoreOptions()
	}
	if err != nil {
		return buildah.StoreOptions{}, "", err
	}
	catalog, err := localstore.DefaultImageDir()
	if err == nil && mode == imageStorePodman {
		catalog, err = selectedStoreCatalog(store)
	}
	return store, catalog, err
}

func commandImageDirectory(cmd *cobra.Command) (string, error) {
	_, dir, err := commandStorage(cmd)
	return dir, err
}
