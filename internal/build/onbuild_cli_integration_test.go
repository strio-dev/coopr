package build

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildConsumesLocalBaseOnBuildARGAndStageDependency(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live rootless Buildah ONBUILD build")
	}
	root := t.TempDir()
	for name, contents := range map[string]string{
		"artifact": "from producer\n",
		"payload":  "from inherited ARG\n",
		"base.coopr": `
from "scratch"
onbuild { arg "filename" "payload" }
onbuild { copy "/artifact" "/inherited" from="producer" }
`,
		"child.coopr": `
from "scratch" as="producer"
copy "artifact" "/artifact"
from "localhost/coopr-inherited-arg:latest"
copy "$filename" "/selected"
`,
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Run(context.Background(), Options{
		File: filepath.Join(root, "base.coopr"), Tag: "localhost/coopr-inherited-arg:latest",
		Platform: "linux/amd64", Format: "docker",
	}); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "child.oci.tar")
	if _, err := Run(context.Background(), Options{
		File: filepath.Join(root, "child.coopr"), Tag: "oci-archive:" + archive,
		Platform: "linux/amd64", Format: "docker",
	}); err != nil {
		t.Fatal(err)
	}
	names := archiveLayerNames(t, archive)
	if !containsName(names, "inherited") || !containsName(names, "selected") {
		t.Fatalf("inherited ONBUILD or authored COPY was lost: %v", names)
	}
}
