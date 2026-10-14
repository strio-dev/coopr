package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	buildahcli "go.podman.io/buildah/pkg/cli"
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
		{[]string{"--timestamp=-1"}, true}, {[]string{"--source-date-epoch=-1"}, true},
		{[]string{"--source-date-epoch="}, true}, {[]string{"--source-date-epoch=0"}, true},
		{[]string{"--cache-ttl=-1h"}, true}, {[]string{"--cache-ttl=invalid"}, false},
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
		if test.valid && cmd.Flags().Changed("source-date-epoch") && test.args[0] == "--source-date-epoch=0" && (epoch == nil || *epoch != 0) {
			t.Fatal("explicit epoch zero lost")
		}
		if test.valid && cmd.Flags().Changed("source-date-epoch") && test.args[0] == "--source-date-epoch=-1" && (epoch == nil || *epoch != -1) {
			t.Fatal("negative epoch lost")
		}
		if cmd.Flags().Changed("timestamp") && flags.Timestamp == 0 && test.valid && (timestamp == nil || *timestamp != 0 || ttl == nil || *ttl != 0) {
			t.Fatal("explicit zeros lost")
		}
		if cmd.Flags().Changed("source-date-epoch") && test.args[0] == "--source-date-epoch=123" && test.valid && (epoch == nil || *epoch != 123 || ttl == nil || *ttl != 2*time.Hour) {
			t.Fatal("explicit epoch or TTL lost")
		}
	}
}

func TestBuildCommandsRejectConflictingControls(t *testing.T) {
	for _, args := range [][]string{
		{"build", "missing.coopr", "--all-platforms", "--platform=linux/amd64"},
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

func TestBuildTimeFlagsEmptyEpochAndTimestampRewrite(t *testing.T) {
	for _, ambient := range []string{"", "456"} {
		t.Run("ambient="+ambient, func(t *testing.T) {
			t.Setenv("SOURCE_DATE_EPOCH", ambient)
			cmd := &cobra.Command{Use: "test"}
			cmd.Flags().Bool("rewrite-timestamp", false, "")
			var flags buildTimeFlags
			flags.addTo(cmd)
			args := []string{"--timestamp=-1", "--rewrite-timestamp"}
			if ambient != "" {
				args = append(args, "--source-date-epoch=")
			}
			if err := cmd.ParseFlags(args); err != nil {
				t.Fatal(err)
			}
			if err := cmd.ValidateFlagGroups(); err != nil {
				t.Fatal(err)
			}
			timestamp, epoch, _, err := flags.values(cmd)
			if err != nil || timestamp == nil || *timestamp != -1 || epoch != nil {
				t.Fatalf("timestamp=%v epoch=%v error=%v", timestamp, epoch, err)
			}
		})
	}
}

func TestBuildTimeFlagsTimestampConflictsWithNumericEpoch(t *testing.T) {
	for _, args := range [][]string{{"--timestamp=1"}, {"--timestamp=1", "--source-date-epoch=2"}} {
		t.Setenv("SOURCE_DATE_EPOCH", "456")
		cmd := &cobra.Command{Use: "test"}
		var flags buildTimeFlags
		flags.addTo(cmd)
		if err := cmd.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := flags.values(cmd); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("args=%v error=%v", args, err)
		}
	}
}

func TestCacheTTLExplicitNoCacheMatchesNativeOptions(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "")
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "Containerfile"), []byte("FROM scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"--cache-ttl=0"}, {"--cache-ttl=0", "--no-cache=false"}, {"--cache-ttl=0", "--no-cache=true"}, {"--cache-ttl=1h", "--no-cache=false"}, {"--cache-ttl=-1s", "--no-cache=false"}, {"--no-cache=false"}, {"--no-cache=true"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			native := &cobra.Command{Use: "native"}
			opts := buildahcli.BuildOptions{LayerResults: new(buildahcli.LayerResults), BudResults: new(buildahcli.BudResults), UserNSResults: new(buildahcli.UserNSResults), FromAndBudResults: new(buildahcli.FromAndBudResults), NameSpaceResults: new(buildahcli.NameSpaceResults)}
			bud := buildahcli.GetBudFlags(opts.BudResults)
			common, err := buildahcli.GetFromAndBudFlags(opts.FromAndBudResults, opts.UserNSResults, opts.NameSpaceResults)
			if err != nil {
				t.Fatal(err)
			}
			layers := buildahcli.GetLayerFlags(opts.LayerResults)
			native.Flags().AddFlagSet(&bud)
			native.Flags().AddFlagSet(&common)
			native.Flags().AddFlagSet(&layers)
			if err := native.ParseFlags(args); err != nil {
				t.Fatal(err)
			}
			expected, _, _, nativeErr := buildahcli.GenBuildOptions(native, []string{contextDir}, opts)
			// GenBuildOptions also stages contexts and returns full Containerfile build
			// options. Only its flag contract applies to Coopr's separate KDL planner.
			cmd := &cobra.Command{Use: "coopr"}
			cmd.Flags().Bool("no-cache", false, "")
			var flags buildTimeFlags
			flags.addTo(cmd)
			if err := cmd.ParseFlags(args); err != nil {
				t.Fatal(err)
			}
			_, _, ttl, err := flags.values(cmd)
			if (err == nil) != (nativeErr == nil) {
				t.Fatalf("Coopr error=%v native error=%v", err, nativeErr)
			}
			if nativeErr != nil && !strings.Contains(nativeErr.Error(), "cannot use --cache-ttl with duration as 0 and --no-cache=false") {
				t.Fatalf("unexpected native error: %v", nativeErr)
			}
			if err == nil && (ttl == nil) != (!native.Flags().Changed("cache-ttl")) {
				t.Fatalf("TTL presence mismatch: %v", ttl)
			}
			if err == nil && ttl != nil && *ttl != expected.CacheTTL {
				t.Fatalf("TTL Coopr=%v native=%v", *ttl, expected.CacheTTL)
			}
		})
	}
}
