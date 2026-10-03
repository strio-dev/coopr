package buildah

import (
	"slices"
	"testing"
)

func TestNativeRuntimeArgs(t *testing.T) {
	flags := []string{"--log-format=json", "--debug"}
	generated := []string{"--bundle", "/tmp/bundle"}
	if got, want := nativeRuntimeArgs(generated, flags, ""), []string{"--log-format=json", "--debug", "--bundle", "/tmp/bundle"}; !slices.Equal(got, want) {
		t.Fatalf("normal runtime args = %q, want %q", got, want)
	}
	if got, want := nativeRuntimeArgs(generated, flags, "/usr/bin/crun"), []string{insecureRuntimeMarker, "/usr/bin/crun", "--log-format=json", "--debug"}; !slices.Equal(got, want) {
		t.Fatalf("insecure runtime args = %q, want %q", got, want)
	}
}
