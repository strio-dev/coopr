package buildah

import "testing"

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
