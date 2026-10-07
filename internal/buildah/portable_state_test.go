package buildah

import (
	"errors"
	"path/filepath"
	"testing"

	"go.podman.io/storage/pkg/mount"
	"golang.org/x/sys/unix"
)

func TestAmbientSELinuxClassification(t *testing.T) {
	mountLabel := "system_u:object_r:container_file_t:s0:c74,c788"
	ambient := storageAmbientSELinux(mountLabel, nil)
	if ambient != "system_u:object_r:container_file_t:s0" {
		t.Fatalf("ambient label = %q", ambient)
	}
	if !matchesAmbientSELinux(mountLabel+"\x00", mountLabel) {
		t.Fatal("current builder MCS label was not recognized as ambient policy")
	}
	if matchesAmbientSELinux("system_u:object_r:container_file_t:s0:c1,c2\x00", mountLabel) {
		t.Fatal("different MCS category matched the current builder label")
	}
	for _, test := range []struct {
		name, label string
		want        bool
	}{
		{"storage root", "system_u:object_r:container_file_t:s0\x00", true},
		{"created file", "unconfined_u:object_r:container_file_t:s0\x00", true},
		{"category change", "system_u:object_r:container_file_t:s0:c1,c2\x00", false},
		{"type change", "system_u:object_r:httpd_sys_content_t:s0\x00", false},
		{"role change", "system_u:system_r:container_file_t:s0\x00", false},
		{"no label", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := matchesAmbientSELinux(test.label, ambient); got != test.want {
				t.Fatalf("matchesAmbientSELinux(%q) = %v, want %v", test.label, got, test.want)
			}
		})
	}
}

func TestStorageAmbientSELinuxFallback(t *testing.T) {
	const inherited = "unconfined_u:object_r:user_home_t:s0:c74,c788"
	for _, test := range []struct {
		name, mountLabel, storageLabel, want string
	}{
		{"unlabeled storage", "", "", ""},
		{"inherited storage", "", inherited, inherited},
		{"terminated inherited storage", "", inherited + "\x00", inherited},
		{"allocated label takes precedence", "system_u:object_r:container_file_t:s0:c1,c2", inherited, "system_u:object_r:container_file_t:s0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := storageAmbientSELinux(test.mountLabel, []byte(test.storageLabel)); got != test.want {
				t.Fatalf("ambient label = %q, want %q", got, test.want)
			}
		})
	}
	if !matchesAmbientSELinux(inherited+"\x00", inherited) {
		t.Fatal("full inherited context did not match")
	}
	for _, changed := range []string{
		"unconfined_u:object_r:user_home_t:s0",
		"unconfined_u:object_r:user_home_t:s0:c1,c2",
		"unconfined_u:object_r:user_home_t:s1:c74,c788",
		"unconfined_u:object_r:httpd_sys_content_t:s0:c74,c788",
		"unconfined_u:system_r:user_home_t:s0:c74,c788",
	} {
		if matchesAmbientSELinux(changed, inherited) {
			t.Fatalf("changed context matched independently observed storage: %q", changed)
		}
	}
	if matchesAmbientSELinux(inherited, storageAmbientSELinux("", nil)) {
		t.Fatal("labeled state was accepted without an independent baseline")
	}
}

func TestNonRelabelableFUSEMount(t *testing.T) {
	const root = "/var/lib/coopr/storage/overlay/layer/merged"
	const graphRoot = "/var/lib/coopr/storage"
	for _, test := range []struct {
		name   string
		change func([]*mount.Info) []*mount.Info
		want   bool
	}{
		{"container FUSE policy", nil, true},
		{"relabelable FUSE superblock", func(m []*mount.Info) []*mount.Info { m[2].VFSOptions = "rw,seclabel"; return m }, false},
		{"mount options are not superblock policy", func(m []*mount.Info) []*mount.Info { m[2].Options = "rw,seclabel"; return m }, true},
		{"different filesystem", func(m []*mount.Info) []*mount.Info { m[2].FSType = "overlay"; return m }, false},
		{"different FUSE filesystem", func(m []*mount.Info) []*mount.Info { m[2].FSType = "fuse.other"; return m }, false},
		{"bound FUSE subtree", func(m []*mount.Info) []*mount.Info { m[2].Root = "/subtree"; return m }, false},
		{"unproven FUSE root", func(m []*mount.Info) []*mount.Info { m[2].Root = ""; return m }, false},
		{"no active SELinux proof", func(m []*mount.Info) []*mount.Info { m[1].VFSOptions = "rw"; return m }, false},
		{"graph mount options alone", func(m []*mount.Info) []*mount.Info { m[1].VFSOptions = "rw"; m[1].Options = "rw,seclabel"; return m }, false},
		{"missing mounted root", func(m []*mount.Info) []*mount.Info { return m[:2] }, false},
		{"containing FUSE mount is insufficient", func(m []*mount.Info) []*mount.Info { m[2].Mountpoint = filepath.Dir(root); return m }, false},
		{"deepest graph mount wins", func(m []*mount.Info) []*mount.Info { m[0].VFSOptions = "rw,seclabel"; m[1].VFSOptions = "rw"; return m }, false},
		{"neighbor is not a containing mount", func(m []*mount.Info) []*mount.Info {
			return append(m, &mount.Info{Mountpoint: graphRoot + "-neighbor", VFSOptions: "rw"})
		}, true},
		{"graphroot exact mount", func(m []*mount.Info) []*mount.Info { m[1].Mountpoint = graphRoot; return m }, true},
		{"root seclabel requires exact token", func(m []*mount.Info) []*mount.Info {
			m[2].VFSOptions = "rw,noseclabel,seclabelled,seclabel=1"
			return m
		}, true},
		{"graph seclabel requires exact token", func(m []*mount.Info) []*mount.Info {
			m[1].VFSOptions = "rw,noseclabel,seclabelled,seclabel=1"
			return m
		}, false},
		{"no mounts", func(m []*mount.Info) []*mount.Info { return nil }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			mounts := []*mount.Info{
				{Mountpoint: "/", Root: "/", FSType: "overlay", VFSOptions: "rw"},
				{Mountpoint: "/var/lib/coopr", Root: "/", FSType: "tmpfs", VFSOptions: "rw,seclabel,size=1048576k"},
				{Mountpoint: root, Root: "/", FSType: "fuse.fuse-overlayfs", Options: "rw,nosuid,nodev,relatime", VFSOptions: "rw,user_id=0,group_id=0,default_permissions,allow_other"},
			}
			if test.change != nil {
				mounts = test.change(mounts)
			}
			if got := nonRelabelableFUSEMount(root, graphRoot, mounts); got != test.want {
				t.Fatalf("nonRelabelableFUSEMount() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestMountedAmbientSELinuxOrdinaryPath(t *testing.T) {
	root := t.TempDir()
	const fallback = "system_u:object_r:container_file_t:s0"
	got, err := mountedAmbientSELinux(root, root, fallback)
	if err != nil {
		t.Fatal(err)
	}
	if got != fallback {
		t.Fatalf("ordinary path changed baseline to %q", got)
	}
	_, err = mountedAmbientSELinux(root+"\x00", root, fallback)
	if !errors.Is(err, unix.EINVAL) {
		t.Fatalf("invalid path error = %v, want EINVAL", err)
	}
}
