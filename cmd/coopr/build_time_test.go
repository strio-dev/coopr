package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestBuildTimeFlagsPreserveExplicitZeroAndValidate(t *testing.T) {
	previous, present := os.LookupEnv("SOURCE_DATE_EPOCH")
	if err := os.Unsetenv("SOURCE_DATE_EPOCH"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv("SOURCE_DATE_EPOCH", previous)
		} else {
			_ = os.Unsetenv("SOURCE_DATE_EPOCH")
		}
	})
	for _, test := range []struct {
		args  []string
		valid bool
	}{
		{nil, true}, {[]string{"--timestamp=0", "--cache-ttl=0"}, true},
		{[]string{"--source-date-epoch=123", "--cache-ttl=2h"}, true},
		{[]string{"--timestamp=-1"}, false}, {[]string{"--source-date-epoch=-1"}, false},
		{[]string{"--cache-ttl=-1h"}, false}, {[]string{"--cache-ttl=invalid"}, false},
	} {
		cmd := &cobra.Command{Use: "test"}
		cmd.Flags().Bool("rewrite-timestamp", false, "")
		var flags buildTimeFlags
		flags.addTo(cmd)
		if err := cmd.ParseFlags(test.args); err != nil {
			t.Fatal(err)
		}
		timestamp, epoch, ttl, err := flags.values(cmd)
		if (err == nil) != test.valid {
			t.Fatalf("args=%v err=%v", test.args, err)
		}
		if len(test.args) == 0 && (timestamp != nil || epoch != nil || ttl != nil) {
			t.Fatal("defaults became explicit values")
		}
		if cmd.Flags().Changed("timestamp") && test.valid && (timestamp == nil || *timestamp != 0 || ttl == nil || *ttl != 0) {
			t.Fatal("explicit zeros lost")
		}
		if cmd.Flags().Changed("source-date-epoch") && test.valid && (epoch == nil || *epoch != 123 || ttl == nil || *ttl != 2*time.Hour) {
			t.Fatal("explicit epoch or TTL lost")
		}
	}
}

func TestBuildCommandsRejectConflictingControls(t *testing.T) {
	for _, args := range [][]string{
		{"build", "missing.coopr", "--all-platforms", "--platform=linux/amd64"},
		{"build", "missing.coopr", "--timestamp=1", "--source-date-epoch=1"},
		{"component", "build", "missing.coopr", "--timestamp=1", "--rewrite-timestamp"},
	} {
		cmd := newRootCommand()
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "if any flags in the group") {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}

func TestBuildTimeFlagsUseAmbientSourceDateEpoch(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "456")
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().Bool("rewrite-timestamp", false, "")
	var flags buildTimeFlags
	flags.addTo(cmd)
	_, epoch, _, err := flags.values(cmd)
	if err != nil || epoch == nil || *epoch != 456 {
		t.Fatalf("ambient epoch = %v, %v", epoch, err)
	}
	if err := cmd.ParseFlags([]string{"--source-date-epoch=123"}); err != nil {
		t.Fatal(err)
	}
	_, epoch, _, err = flags.values(cmd)
	if err != nil || epoch == nil || *epoch != 123 {
		t.Fatalf("explicit epoch = %v, %v", epoch, err)
	}
	t.Setenv("SOURCE_DATE_EPOCH", "invalid")
	cmd = &cobra.Command{Use: "test"}
	cmd.Flags().Bool("rewrite-timestamp", false, "")
	flags = buildTimeFlags{}
	flags.addTo(cmd)
	if _, _, _, err := flags.values(cmd); err == nil || !strings.Contains(err.Error(), "SOURCE_DATE_EPOCH") {
		t.Fatalf("invalid ambient epoch accepted: %v", err)
	}
}
