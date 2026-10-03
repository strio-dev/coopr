package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	imageStoreCoopr  = "coopr"
	imageStorePodman = "podman"
)

type cooprConfig struct {
	ImageStore string `toml:"image-store"`
}

func loadCooprConfig() (cooprConfig, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return cooprConfig{}, fmt.Errorf("find user configuration directory: %w", err)
	}
	path := filepath.Join(dir, "coopr", "config.toml")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cooprConfig{ImageStore: imageStoreCoopr}, nil
	}
	if err != nil {
		return cooprConfig{}, fmt.Errorf("read Coopr configuration %s: %w", path, err)
	}
	var config cooprConfig
	metadata, err := toml.Decode(string(data), &config)
	if err != nil {
		return cooprConfig{}, fmt.Errorf("decode Coopr configuration %s: %w", path, err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) != 0 {
		return cooprConfig{}, fmt.Errorf("coopr configuration %s has unknown key %q", path, undecoded[0].String())
	}
	if config.ImageStore == "" && !metadata.IsDefined("image-store") {
		config.ImageStore = imageStoreCoopr
	}
	config.ImageStore = strings.ToLower(config.ImageStore)
	if err := validateImageStore(config.ImageStore); err != nil {
		return cooprConfig{}, fmt.Errorf("coopr configuration %s: %w", path, err)
	}
	return config, nil
}

func validateImageStore(value string) error {
	switch value {
	case imageStoreCoopr, imageStorePodman:
		return nil
	default:
		return fmt.Errorf("image store must be %q or %q, got %q", imageStoreCoopr, imageStorePodman, value)
	}
}
