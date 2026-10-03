package buildah

import (
	"testing"

	buildahcli "go.podman.io/buildah/pkg/cli"
	buildahparse "go.podman.io/buildah/pkg/parse"
)

func TestNonNegativeBytesMatchesBuildahUnits(t *testing.T) {
	for _, value := range []string{"0", "0b", "0.1", "1", "1b", "64m", "1K", "1kb", "1KiB", "1.5 GiB", "1.5gb", "2t", "1PiB", "1e3k", "0.5k"} {
		flags, err := buildahcli.GetFromAndBudFlags(&buildahcli.FromAndBudResults{}, &buildahcli.UserNSResults{}, &buildahcli.NameSpaceResults{})
		if err != nil {
			t.Fatal(err)
		}
		if err := flags.Set("memory", value); err != nil {
			t.Fatal(err)
		}
		upstream, err := buildahparse.CommonBuildOptionsFromFlagSet(&flags, flags.Lookup)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseNonNegativeBytes("memory", value)
		if err != nil || got != upstream.Memory {
			t.Fatalf("value=%q got=%d Buildah=%d err=%v", value, got, upstream.Memory, err)
		}
	}
	for _, value := range []string{"", "-1m", "1mc", "1kibx", "1bb", "1ib", "NaN", "Inf", "1e100p", " 1k", "1 k ", "1e", "1e3e"} {
		if _, err := parseNonNegativeBytes("memory", value); err == nil {
			t.Fatalf("invalid size %q accepted", value)
		}
	}
}

func TestNonNegativeBytesIgnoresUnrelatedHostConfiguration(t *testing.T) {
	t.Setenv("CONTAINERS_CONF", "/proc/self/status")
	got, err := parseNonNegativeBytes("memory", "64m")
	if err != nil || got != 64<<20 {
		t.Fatalf("size=%d err=%v", got, err)
	}
}
