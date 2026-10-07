package build

import (
	"archive/tar"
	"bytes"
	"context"
	"coopr/internal/buildah"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"coopr/internal/buildcontext"
	"coopr/internal/oci"
	"coopr/internal/transfer"
)

func TestBuildUsesRemotePrimaryHTTPContext(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live Buildah tests")
	}
	archive := &bytes.Buffer{}
	tarWriter := tar.NewWriter(archive)
	contents := []byte("remote primary context\n")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "proof", Mode: 0o644, Size: int64(len(contents)), ModTime: time.Unix(946684800, 0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(archive.Bytes())
	}))
	defer server.Close()
	dir := t.TempDir()
	definition := filepath.Join(dir, "app.coopr")
	if err := os.WriteFile(definition, []byte("arg \"SOURCE_DATE_EPOCH\" \"context\"\nfrom \"scratch\"\ncopy \"proof\" \"/proof\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "image.oci.tar")
	if _, err := Run(context.Background(), Options{
		File: definition, Context: server.URL + "/context.tar", Tag: "oci-archive:" + output, Platform: "linux/amd64",
	}); err != nil {
		t.Fatal(err)
	}
	if names := archiveLayerNames(t, output); !containsName(names, "proof") {
		t.Fatalf("remote primary context file missing from image layers: %v", names)
	}
	config := archiveImageConfig(t, output)
	if config.Created == nil || config.Created.Unix() != 946684800 {
		t.Fatalf("image creation time = %v, want 946684800", config.Created)
	}
}

func TestBuildUsesNamedLocalContextsForFromAndCopy(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live Buildah tests")
	}
	root := t.TempDir()
	base := filepath.Join(root, "base")
	assets := filepath.Join(root, "assets")
	for _, dir := range []string{base, assets} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(base, "base-marker"):     "base\n",
		filepath.Join(assets, "payload"):       "asset\n",
		filepath.Join(assets, "ignored"):       "skip\n",
		filepath.Join(assets, ".dockerignore"): "ignored\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(root, "app.coopr")
	if err := os.WriteFile(file, []byte("from \"base\"\ncopy \"payload\" \"/payload\" from=\"assets\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "image.oci.tar")
	_, err := Run(context.Background(), Options{
		File: file, Tag: "oci-archive:" + archive, Platform: "linux/amd64",
		BuildContexts: []buildcontext.Spec{
			{Name: "base", Kind: buildcontext.Local, Path: base},
			{Name: "assets", Kind: buildcontext.Local, Path: assets},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	names := archiveLayerNames(t, archive)
	for _, name := range []string{"base-marker", "payload"} {
		if !containsName(names, name) {
			t.Fatalf("named context file %q missing from image layers: %v", name, names)
		}
	}
	if containsName(names, "ignored") {
		t.Fatalf("named context .dockerignore did not filter the source: %v", names)
	}
}

func TestBuildCopyDotExcludesStagingInsideContext(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live Buildah tests")
	}
	contextDir := t.TempDir()
	t.Setenv("TMPDIR", contextDir)
	definition := filepath.Join(contextDir, "app.coopr")
	if err := os.WriteFile(definition, []byte("from \"scratch\"\ncopy \".\" \"/captured/\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "marker"), []byte("authored\n"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "image.oci.tar")
	if _, err := Run(context.Background(), Options{File: definition, Tag: "oci-archive:" + archive, Platform: "linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	names := archiveLayerNames(t, archive)
	if !containsName(names, "captured/marker") {
		t.Fatalf("COPY . lost authored marker: %v", names)
	}
	for _, name := range names {
		if strings.Contains(name, ".coopr-stage-") || strings.Contains(name, ".coopr-build-worker-") {
			t.Fatalf("COPY . captured Coopr-owned context artifact %q", name)
		}
	}
}

func TestCopyURLFailsWithoutNetworkRequest(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live Buildah tests")
	}
	var sourceRequests atomic.Int32
	var requestDetails struct {
		sync.Mutex
		values []string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.EscapedPath() == "/payload" {
			sourceRequests.Add(1)
			requestDetails.Lock()
			requestDetails.values = append(requestDetails.values, request.Method+" "+request.URL.RequestURI()+" user-agent="+request.UserAgent())
			requestDetails.Unlock()
		}
		_, _ = w.Write([]byte("remote payload"))
	}))
	defer server.Close()
	dir := t.TempDir()
	definition := filepath.Join(dir, "app.coopr")
	content := "from \"scratch\"\ncopy \"" + server.URL + "/payload\" \"/payload\"\n"
	if err := os.WriteFile(definition, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "image.oci.tar")
	_, err := Run(context.Background(), Options{File: definition, Tag: "oci-archive:" + archive, Platform: "linux/amd64"})
	if err == nil || !strings.Contains(err.Error(), "COPY source must be local") {
		t.Fatalf("COPY remote source error = %v", err)
	}
	if got := sourceRequests.Load(); got != 0 {
		requestDetails.Lock()
		details := append([]string(nil), requestDetails.values...)
		requestDetails.Unlock()
		t.Fatalf("COPY issued %d source requests: %v", got, details)
	}
}

func TestBuiltBaseAndFromShareCanonicalStore(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for live Buildah tests")
	}
	root := t.TempDir()
	baseDefinition := filepath.Join(root, "base.coopr")
	if err := os.WriteFile(baseDefinition, []byte("from \"scratch\"\ncopy \"base\" \"/base\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "base"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	const baseTag = "localhost/coopr-store-proof:dev"
	if _, err := Run(context.Background(), Options{File: baseDefinition, Tag: baseTag, Platform: "linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	nativeStore, err := buildah.DefaultStoreOptions()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"oci-layout", "index.json", "blobs"} {
		if _, err := os.Stat(filepath.Join(nativeStore.GraphRoot, name)); !os.IsNotExist(err) {
			t.Fatalf("built image left a second persistent OCI store entry %q: %v", name, err)
		}
	}
	copyArchive := filepath.Join(t.TempDir(), "base-copy.oci.tar")
	copyDestination, err := transfer.ParseDestination("oci-archive:"+copyArchive, oci.Image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transfer.Copy(context.Background(), oci.Image, baseTag, copyDestination, transfer.Options{}); err != nil {
		t.Fatalf("copy built image from canonical store: %v", err)
	}
	if !containsName(archiveLayerNames(t, copyArchive), "base") {
		t.Fatal("copied image lost the base file")
	}
	const alias = "localhost/coopr-store-proof:alias"
	aliasDestination, err := transfer.ParseDestination(alias, oci.Image)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := transfer.Copy(context.Background(), oci.Image, baseTag, aliasDestination, transfer.Options{}); err != nil || got != alias {
		t.Fatalf("tag stored image as %q: got %q, %v", alias, got, err)
	}
	childDefinition := filepath.Join(root, "child.coopr")
	if err := os.WriteFile(childDefinition, []byte("from \""+alias+"\"\ncopy \"child\" \"/child\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "child"), []byte("child\n"), 0600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "child.oci.tar")
	if _, err := Run(context.Background(), Options{File: childDefinition, Tag: "oci-archive:" + archive, Platform: "linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	names := archiveLayerNames(t, archive)
	if !containsName(names, "base") || !containsName(names, "child") {
		t.Fatalf("child lost its local base or own file: %v", names)
	}
	namedDefinition := filepath.Join(root, "named-child.coopr")
	if err := os.WriteFile(namedDefinition, []byte("from \"base\"\ncopy \"child\" \"/child\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	namedArchive := filepath.Join(t.TempDir(), "named-child.oci.tar")
	if _, err := Run(context.Background(), Options{
		File: namedDefinition, Tag: "oci-archive:" + namedArchive, Platform: "linux/amd64",
		BuildContexts: []buildcontext.Spec{{Name: "base", Kind: buildcontext.DockerImage, Reference: alias}},
	}); err != nil {
		t.Fatalf("build with local image named context: %v", err)
	}
	namedNames := archiveLayerNames(t, namedArchive)
	if !containsName(namedNames, "base") || !containsName(namedNames, "child") {
		t.Fatalf("named image context lost base or child file: %v", namedNames)
	}
}
