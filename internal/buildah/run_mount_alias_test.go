package buildah

import (
	"slices"
	"strings"
	"testing"
)

func TestDirectBindOptionsPreserveNativeBooleanSpellings(t *testing.T) {
	for _, flag := range []struct{ name, option string }{
		{"bind-nonrecursive", "bind"}, {"nosuid", "nosuid"}, {"nodev", "nodev"}, {"noexec", "noexec"},
		{"private", "private"}, {"rprivate", "rprivate"}, {"u", "U"}, {"no-dereference", "no-dereference"},
	} {
		for _, value := range []string{"true", "1", "TRUE", "T", "True", "false", "0", "FALSE", "F", "False"} {
			t.Run(flag.name+"/"+value, func(t *testing.T) {
				properties := map[string]string{"target": "/input", flag.name: value}
				if _, err := serializeRunMount(RunMount{Type: "bind", Properties: properties}, "/context"); err != nil {
					t.Fatalf("native boolean rejected: %v", err)
				}
				options := writableContextBindOptions(properties)
				want := value == "true" || value == "1" || value == "TRUE" || value == "T" || value == "True"
				if slices.Contains(options, flag.option) != want {
					t.Fatalf("%s=%s: options %v, want %s present=%t", flag.name, value, options, flag.option, want)
				}
			})
		}
	}
}

func TestRunMountAliasesMatchDockerfileSecretAndWritableForms(t *testing.T) {
	for _, test := range []struct {
		name  string
		mount RunMount
		want  []string
	}{
		{
			name:  "secret source alias",
			mount: RunMount{Type: "secret", Properties: map[string]string{"src": "token", "env": "TOKEN", "rw": "true"}},
			want:  []string{"type=secret", "id=token", "env=TOKEN"},
		},
		{
			name:  "cache writable alias",
			mount: RunMount{Type: "cache", Properties: map[string]string{"id": "cache", "dst": "/cache", "readwrite": "true"}},
			want:  []string{"type=cache", "id=cache", "target=/cache"},
		},
		{
			name:  "tmpfs writable alias",
			mount: RunMount{Type: "tmpfs", Properties: map[string]string{"destination": "/tmp", "rw": "true"}},
			want:  []string{"type=tmpfs", "target=/tmp"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			serialized, err := serializeRunMount(test.mount, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range test.want {
				if !strings.Contains(serialized, field) {
					t.Fatalf("mount %q does not contain %q", serialized, field)
				}
			}
			if strings.Contains(serialized, "readwrite") || strings.Contains(serialized, "readonly") {
				t.Fatalf("redundant writable option in mount %q", serialized)
			}
		})
	}
	for _, properties := range []map[string]string{
		{"source": "one", "id": "two"},
		{"src": "one", "source": "two"},
	} {
		if _, err := serializeRunMount(RunMount{Type: "secret", Properties: properties}, ""); err == nil {
			t.Fatalf("accepted conflicting secret source properties %#v", properties)
		}
	}
}
