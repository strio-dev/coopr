package build

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRunMountOnlyDefinitionWithoutProjectLock(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live Buildah tests")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "app.coopr")
	source := "from \"" + exampleBase + "\"\nrun \"echo marker > /out\" network=\"none\" { mount \"cache\" target=\"/cache\" }\n"
	if err := os.WriteFile(file, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		output := filepath.Join(dir, []string{"first.oci.tar", "second.oci.tar"}[i])
		if _, err := Run(context.Background(), Options{File: file, Context: dir, Tag: "oci-archive:" + output, Platform: "linux/amd64"}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(output); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "coopr.lock")); !os.IsNotExist(err) {
		t.Fatalf("build created a project lockfile: %v", err)
	}
}
