package transfer

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestSigningContextArtifactsUsesAmbientGPGHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, custom := range []string{"", filepath.Join(home, "custom")} {
		t.Setenv("GNUPGHOME", custom)
		paths, err := (SigningOptions{SignBy: "fingerprint"}).ContextArtifacts()
		if err != nil {
			t.Fatal(err)
		}
		want := custom
		if want == "" {
			want = filepath.Join(home, ".gnupg")
		}
		if !slices.Contains(paths, want) {
			t.Fatalf("paths=%v want=%s", paths, want)
		}
	}
	paths, err := (SigningOptions{}).ContextArtifacts()
	if err != nil || len(paths) != 2 {
		t.Fatalf("inactive signing paths=%v err=%v", paths, err)
	}
}
