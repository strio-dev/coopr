package main

import (
	"testing"

	"coopr/internal/transfer"

	"github.com/spf13/cobra"
)

func TestPushTransferFlagsNativeOptions(t *testing.T) {
	for _, test := range []struct {
		args  []string
		valid bool
	}{
		{[]string{"--format=oci", "--compression-format=zstd", "--compression-level=3", "--remove-signatures"}, true},
		{[]string{"--format=v2s2"}, true},
		{[]string{"--format=invalid"}, false},
		{[]string{"--compression-format=invalid"}, false},
		{[]string{"--encrypt-layer=0"}, false},
		{[]string{"--force-compression"}, true},
	} {
		cmd := &cobra.Command{Use: "push"}
		var flags pushTransferFlags
		flags.addTo(cmd, false)
		if err := cmd.ParseFlags(test.args); err != nil {
			t.Fatal(err)
		}
		var options transfer.Options
		err := flags.apply(cmd, &options)
		if (err == nil) != test.valid {
			t.Fatalf("%v error=%v", test.args, err)
		}
		if test.valid && cmd.Flags().Changed("compression-format") && (!options.Push.ForceCompression || options.Push.CompressionLevel == nil || *options.Push.CompressionLevel != 3 || !options.Push.RemoveSignatures) {
			t.Fatalf("options=%+v", options.Push)
		}
	}
}
