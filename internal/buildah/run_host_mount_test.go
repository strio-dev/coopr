package buildah

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

func TestLiveHostMountUsesDirectBindAndNeverContextSnapshot(t *testing.T) {
	for _, test := range []struct {
		name       string
		properties map[string]string
		readonly   bool
		relabel    string
	}{
		{"readonly", map[string]string{"target": "/live", "readonly": "true"}, true, ""},
		{"writable", map[string]string{"target": "/live", "readonly": "false"}, false, ""},
		{"default writable", map[string]string{"target": "/live"}, false, ""},
		{"readonly aliases", map[string]string{"dst": "/live", "ro": "true", "z": "true"}, true, "z"},
		{"writable aliases", map[string]string{"destination": "/live", "readwrite": "true", "Z": "true"}, false, "Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			mount := RunMount{Type: "bind", HostPath: directory, Properties: test.properties}
			builder := &recordingBuilder{}
			if err := (Run{Command: []string{"true"}, Mounts: []RunMount{mount}}).apply(builder, ""); err != nil {
				t.Fatal(err)
			}
			options := builder.runOptions[0]
			if len(options.RunMounts) != 0 || len(options.Mounts) != 1 || options.Mounts[0].Source != directory || options.Mounts[0].Destination != "/live" {
				t.Fatalf("live source changed: %#v", options)
			}
			if slices.Contains(options.Mounts[0].Options, "ro") != test.readonly {
				t.Fatalf("access = %v", options.Mounts[0].Options)
			}
			if slices.Contains(options.Mounts[0].Options, "z") != (test.relabel == "z") || slices.Contains(options.Mounts[0].Options, "Z") != (test.relabel == "Z") {
				t.Fatalf("relabel = %v", options.Mounts[0].Options)
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
	mounts := []RunMount{
		{Type: "bind", HostPath: filepath.Join(root, "missing"), Properties: map[string]string{"target": "/live"}},
		{Type: "bind", HostPath: "relative", Properties: map[string]string{"target": "/live"}},
		{Type: "bind", HostPath: root, Properties: map[string]string{"target": "/live", "from": "image"}},
		{Type: "bind", HostPath: root, Properties: map[string]string{"target": "/live", "source": "child"}},
	}
	for _, name := range []string{"source", "from", "bind-nonrecursive", "nosuid", "nodev", "noexec", "shared", "rshared", "private", "rprivate", "slave", "rslave", "u", "U", "no-dereference", "bind-propagation", "consistency"} {
		for _, value := range []string{"", "false"} {
			mounts = append(mounts, RunMount{Type: "bind", HostPath: root, Properties: map[string]string{"target": "/live", name: value}})
		}
	}
	for _, properties := range []map[string]string{
		{"target": "/live", "dst": "/live"},
		{"target": "/live", "readonly": "invalid"},
		{"target": "/live", "relabel": "invalid"},
	} {
		mounts = append(mounts, RunMount{Type: "bind", HostPath: root, Properties: properties})
	}
	for _, mount := range mounts {
		builder := &recordingBuilder{}
		if err := (Run{Command: []string{"true"}, Mounts: []RunMount{mount}}).apply(builder, ""); err == nil || len(builder.events) != 0 {
			t.Fatalf("invalid live bind executed: %#v, %v", mount, err)
		}
	}
}
