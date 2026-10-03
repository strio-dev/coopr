package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func TestGlobalLogLevel(t *testing.T) {
	for _, test := range []struct {
		name  string
		args  []string
		level logrus.Level
	}{
		{name: "default", level: logrus.WarnLevel},
		{name: "warn", args: []string{"--log-level=warn"}, level: logrus.WarnLevel},
		{name: "warning", args: []string{"--log-level=warning"}, level: logrus.WarnLevel},
		{name: "info", args: []string{"--log-level=info"}, level: logrus.InfoLevel},
		{name: "debug", args: []string{"--log-level=debug"}, level: logrus.DebugLevel},
		{name: "trace", args: []string{"--log-level=trace"}, level: logrus.TraceLevel},
		{name: "error", args: []string{"--log-level=error"}, level: logrus.ErrorLevel},
		{name: "fatal", args: []string{"--log-level=fatal"}, level: logrus.FatalLevel},
		{name: "panic", args: []string{"--log-level=panic"}, level: logrus.PanicLevel},
	} {
		t.Run(test.name, func(t *testing.T) {
			logger := logrus.StandardLogger()
			level, output, hooks := logger.GetLevel(), logger.Out, logger.ReplaceHooks(make(logrus.LevelHooks))
			t.Cleanup(func() {
				logger.SetLevel(level)
				logger.SetOutput(output)
				logger.ReplaceHooks(hooks)
			})
			var oldOutput, stdout, stderr bytes.Buffer
			logger.SetLevel(logrus.TraceLevel)
			logger.SetOutput(&oldOutput)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			root := newRootCommand()
			ran := false
			root.AddCommand(&cobra.Command{Use: "log-probe", Run: func(cmd *cobra.Command, _ []string) {
				ran = true
				logrus.Trace("coopr-test-trace")
				logrus.Debug("coopr-test-debug")
				logrus.Info("coopr-test-info")
				logrus.Warn("coopr-test-warning")
				logrus.Error("coopr-test-error")
			}})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs(append(append([]string(nil), test.args...), "log-probe"))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if !ran {
				t.Fatal("logging probe did not execute")
			}
			if got := logger.GetLevel(); got != test.level {
				t.Errorf("log level = %v, want %v", got, test.level)
			}
			for _, message := range []struct {
				level logrus.Level
				text  string
			}{
				{logrus.TraceLevel, "coopr-test-trace"},
				{logrus.DebugLevel, "coopr-test-debug"},
				{logrus.InfoLevel, "coopr-test-info"},
				{logrus.WarnLevel, "coopr-test-warning"},
				{logrus.ErrorLevel, "coopr-test-error"},
			} {
				if got, want := strings.Contains(stderr.String(), message.text), message.level <= test.level; got != want {
					t.Errorf("stderr contains %q = %v, want %v; stderr = %q", message.text, got, want, stderr.String())
				}
			}
			if stdout.Len() != 0 || oldOutput.Len() != 0 {
				t.Errorf("logs escaped stderr: stdout = %q, previous output = %q", stdout.String(), oldOutput.String())
			}
		})
	}
}

func TestInvalidLogLevelPrecedesStorageConfiguration(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configDir := filepath.Join(configHome, "coopr")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("invalid TOML ["), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if status := run([]string{"--log-level=invalid", "images"}, &stdout, &stderr); status == 0 {
		t.Fatal("invalid log level succeeded")
	}
	if !strings.Contains(stderr.String(), "--log-level") || !strings.Contains(stderr.String(), "invalid") || strings.Contains(stderr.String(), "configuration") || strings.Contains(stderr.String(), "unknown flag") {
		t.Fatalf("expected log-level validation before configuration, got %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("invalid log level wrote stdout: %q", stdout.String())
	}
}
