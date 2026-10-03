package buildah

import (
	"strings"
	"testing"
)

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
