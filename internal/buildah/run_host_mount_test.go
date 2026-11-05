package buildah

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

func TestLiveHostMountUsesDirectBindAndNeverContextSnapshot(t *testing.T) {
	for _, readonly := range []string{"true", "false"} {
		t.Run(readonly, func(t *testing.T) {
			directory := t.TempDir()
			mount := RunMount{Type: "bind", HostPath: directory, Properties: map[string]string{"target": "/live", "readonly": readonly}}
			builder := &recordingBuilder{}
			if err := (Run{Command: []string{"true"}, Mounts: []RunMount{mount}}).apply(builder, ""); err != nil {
				t.Fatal(err)
			}
			options := builder.runOptions[0]
			if len(options.RunMounts) != 0 || len(options.Mounts) != 1 || options.Mounts[0].Source != directory {
				t.Fatalf("live source changed: %#v", options)
			}
			if slices.Contains(options.Mounts[0].Options, "ro") != (readonly == "true") {
				t.Fatalf("access = %v", options.Mounts[0].Options)
			}
			if !hasLiveHostMount([]RunMount{mount}) || hasContextBindMount([]RunMount{mount}) || hasWritableContextBindMount([]RunMount{mount}) {
				t.Fatal("live host bind misclassified as snapshot input")
			}
			if _, err := prepareRunInput(context.Background(), builder, "", nil, Run{Mounts: []RunMount{mount}}); err == nil {
				t.Fatal("live input admitted to instruction cache")
			}
		})
	}
}

func TestLiveHostMountRejectsUnavailableAndConflictingSources(t *testing.T) {
	root := t.TempDir()
	for _, mount := range []RunMount{
		{Type: "bind", HostPath: filepath.Join(root, "missing"), Properties: map[string]string{"target": "/live"}},
		{Type: "bind", HostPath: "relative", Properties: map[string]string{"target": "/live"}},
		{Type: "bind", HostPath: root, Properties: map[string]string{"target": "/live", "from": "image"}},
		{Type: "bind", HostPath: root, Properties: map[string]string{"target": "/live", "source": "child"}},
	} {
		builder := &recordingBuilder{}
		if err := (Run{Command: []string{"true"}, Mounts: []RunMount{mount}}).apply(builder, ""); err == nil || len(builder.events) != 0 {
			t.Fatalf("invalid live bind executed: %#v, %v", mount, err)
		}
	}
}
