package main

import (
	"strings"
	"testing"
)

func TestNativeSigningAndArtifactFlags(t *testing.T) {
	for _, command := range [][]string{{"push"}, {"manifest", "push"}} {
		cmd, _, err := newRootCommand().Find(command)
		if err != nil {
			t.Fatal(err)
		}
		for _, flag := range []string{"sign-by-sigstore", "sign-by-sq-fingerprint"} {
			if cmd.Flags().Lookup(flag) == nil {
				t.Errorf("%v missing --%s", command, flag)
			}
		}
	}
	cmd := newManifestAddCommand()
	for _, flag := range []string{"artifact", "artifact-type", "artifact-config", "artifact-config-type", "artifact-layer-type", "artifact-subject", "artifact-exclude-titles", "artifact-annotation"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("manifest add missing --%s", flag)
		}
	}
}

func TestArtifactFlagsRequireArtifactBeforeOpeningStore(t *testing.T) {
	cmd := newManifestAddCommand()
	if err := cmd.ParseFlags([]string{"--artifact-type", "text/plain"}); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, []string{"missing", "file"}); err == nil || !strings.Contains(err.Error(), "requires --artifact") {
		t.Fatalf("got %v", err)
	}
}
