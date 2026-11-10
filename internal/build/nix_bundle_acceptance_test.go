package build

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	registryserver "github.com/google/go-containerregistry/pkg/registry"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/oci"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// The acceptance harness supplies a real Nix-produced bundle. Ordinary short
// checks cannot stand in for this native, provisioned runtime qualification.
func TestConsumeNixCompleteBundle(t *testing.T) {
	input := os.Getenv("COOPR_NIX_BUNDLE")
	if input == "" {
		t.Skip("requires a real Nix component bundle from the acceptance harness")
	}
	if testing.Short() {
		t.Fatal("real Nix bundle qualification cannot use short mode")
	}
	parentScratch := os.Getenv("COOPR_NIX_CONSUMER_SCRATCH")
	if !filepath.IsAbs(parentScratch) {
		t.Fatal("qualification requires an explicit private scratch parent")
	}
	root, err := os.MkdirTemp(parentScratch, "consumer-native-")
	if err != nil {
		t.Fatal(err)
	}
	// Preserve this explicitly owned fixture for acceptance evidence. Native Nix
	// layers can contain read-only directories; no user-data cleanup is attempted.
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	server := httptest.NewServer(registryserver.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	client, err := bale.NewClient(bale.ClientOptions{PlainHTTPRegistries: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	archive := os.Getenv("COOPR_NIX_BUNDLE_ARCHIVE")
	if !filepath.IsAbs(archive) {
		t.Fatal("qualification requires an explicit complete-archive output")
	}
	if _, err := client.Save(t.Context(), input, archive); err != nil {
		t.Fatal(err)
	}
	received := filepath.Join(root, "received-bundle")
	if _, err := client.Load(t.Context(), archive, received); err != nil {
		t.Fatal(err)
	}
	input = received
	report, err := client.InspectLayout(t.Context(), input)
	if err != nil || report.MetadataOnly || len(report.Metadata.Roots) == 0 {
		t.Fatalf("invalid real input bundle: %+v, %v", report, err)
	}
	parent := report.Metadata.Roots[0]
	if parent.Descriptor.Digest.String() != os.Getenv("COOPR_NIX_PARENT_DIGEST") {
		t.Fatal("bundle parent differs from the actual producer metadata")
	}
	plan, err := client.PlanPush(t.Context(), input, host+"/nix/delivery:run", bale.PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := client.Push(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	consumerStore := filepath.Join(root, "received-components")
	receiver, err := oci.NewResolver(oci.Options{ComponentStoreDir: consumerStore, TLSVerify: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	inputStore, err := orasoci.NewFromFS(t.Context(), os.DirFS(input))
	if err != nil {
		t.Fatal(err)
	}
	components := 0
	for _, entry := range delivered.Relocations {
		if entry.Descriptor.MediaType != v1.MediaTypeImageManifest {
			continue
		}
		raw, err := content.FetchAll(t.Context(), inputStore, entry.Descriptor)
		if err != nil {
			t.Fatal(err)
		}
		var manifest v1.Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.ArtifactType != oci.ComponentArtifactType {
			continue
		}
		if _, err := receiver.PullComponent(t.Context(), entry.Destination.Repository+"@"+entry.Descriptor.Digest.String(), ""); err != nil {
			t.Fatal(err)
		}
		components++
	}
	if components < 2 {
		t.Fatalf("complete bundle did not provide parent and selected child: %d", components)
	}
	placement, ok := delivered.Relocations.Lookup(bale.ArtifactID{Repository: parent.Repository, Digest: parent.Descriptor.Digest})
	if !ok {
		t.Fatal("parent has no delivery placement")
	}
	selectedContext := os.Getenv("COOPR_NIX_EXPECTED_DELIVERY_CONTEXT")
	var selectedChild digest.Digest
	if selectedContext != "" {
		platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
		resolved, err := receiver.ResolveComponent(t.Context(), placement.Destination.Repository+"@"+parent.Descriptor.Digest.String(), platform, nil, nil)
		if err != nil || resolved.Delivery == nil {
			t.Fatalf("parent delivery selection: %+v, %v", resolved, err)
		}
		childRef := os.Getenv("COOPR_NIX_EXPECTED_CHILD_REFERENCE")
		expectedChild := digest.Digest(childRef[strings.LastIndex(childRef, "@")+1:])
		if err := expectedChild.Validate(); err != nil {
			t.Fatal(err)
		}
		mapped, err := resolved.Delivery.MapExternalInput("component", childRef)
		if err != nil || mapped == childRef {
			t.Fatalf("selected child was not relocated: %s, %v", mapped, err)
		}
		scope, err := resolved.Delivery.DependencyScope(childRef)
		if err != nil || scope == nil || scope.Policy.Select == nil || scope.Policy.Select.Context != selectedContext {
			t.Fatalf("child declaration policy changed: %+v, %v", scope, err)
		}
		child, err := receiver.ResolveComponent(t.Context(), mapped, platform, nil, scope)
		if err != nil || child.Root.Digest != expectedChild || child.Delivery == nil || child.Delivery.Resolution.Declaration == nil || child.Delivery.Resolution.Declaration.Context != selectedContext {
			t.Fatalf("child declaration selection changed: %+v, %v", child.Delivery, err)
		}
		selectedChild = child.Root.Digest
	}
	// No registry is available during either container build. There are no
	// producer stores, archives or named Nix contexts supplied to the consumer.
	server.Close()
	context := filepath.Join(root, "caller")
	if err := os.Mkdir(context, 0700); err != nil {
		t.Fatal(err)
	}
	definition := filepath.Join(context, "image.coopr")
	source := fmt.Sprintf(`arg "EXPECTED_LITERAL"
from "scratch" {
    arg "EXPECTED_LITERAL"
    env EXPECTED_LITERAL="${EXPECTED_LITERAL}" CALLER_MARK="retained" PATH="/caller/bin:/bin"
    workdir "/caller"
    cmd { exec "/caller-command"; }
    component %q
    run {
        exec "/bin/bash" "-euc" "test -L /bin/bash; test -x \"$CHILD_TOOL\"; test \"$CALLER_MARK\" = retained; test \"$CHILD_CONFIG\" = retained; test \"$PWD\" = /caller; test \"$(cat /proof; printf .)\" = \"$EXPECTED_LITERAL.\"; test \"$(cat /hello)\" = 'Hello, world!'; echo NIX_COMPONENT_CONSUMER_RAN"
    }
}
`, placement.Destination.Repository+"@"+parent.Descriptor.Digest.String())
	if err := os.WriteFile(definition, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	store := nativeBuildTestStore(filepath.Join(root, "images"))
	controls, err := buildah.ParseRunControls(buildah.RunControlInput{Runtime: os.Getenv("COOPR_NIX_RUNTIME"), Isolation: "rootless"})
	if err != nil {
		t.Fatal(err)
	}
	stamp := int64(1)
	var imageID string
	var manifest digest.Digest
	var builds []map[string]any
	for _, phase := range []string{"cold", "warm"} {
		var output, stderr bytes.Buffer
		result, err := Run(t.Context(), Options{File: definition, Context: context, Platform: "linux/" + runtime.GOARCH, Tag: "nix-delivery:run", Timestamp: &stamp, BuildStore: store, ComponentStoreDir: consumerStore, CacheLocalDir: filepath.Join(root, "cache"), PullPolicy: "never", Network: "none", RunControls: controls, Args: map[string]string{"EXPECTED_LITERAL": os.Getenv("COOPR_NIX_EXPECTED_LITERAL")}, Stdout: &output, Stderr: &stderr})
		if err != nil {
			t.Fatalf("%s native consumer: %v\n%s\n%s", phase, err, &output, &stderr)
		}
		childRan := strings.Contains(output.String(), "NIX_COMPONENT_CHILD_RAN\n")
		consumerRan := strings.Contains(output.String(), "NIX_COMPONENT_CONSUMER_RAN\n")
		if phase == "cold" && (!childRan || !consumerRan) {
			t.Fatalf("cold consumer skipped real execution: %s", &output)
		}
		if phase == "warm" && (childRan || consumerRan || result.ImageID != imageID) {
			t.Fatalf("warm consumer changed result or reexecuted: %+v\n%s", result, &output)
		}
		selection, found, err := testStoredImageSelection(t.Context(), store, "nix-delivery:run", v1.Platform{OS: "linux", Architecture: runtime.GOARCH})
		if err != nil || !found {
			t.Fatalf("inspect native image: %v, found=%t", err, found)
		}
		if phase == "warm" && selection.Manifest.Digest != manifest {
			t.Fatalf("warm manifest changed: %s != %s", selection.Manifest.Digest, manifest)
		}
		var config v1.Image
		if err := json.Unmarshal(selection.ConfigData, &config); err != nil {
			t.Fatal(err)
		}
		if config.Config.WorkingDir != "/caller" || !slices.Equal(config.Config.Cmd, []string{"/caller-command"}) || !slices.Contains(config.Config.Env, "CALLER_MARK=retained") || !slices.Contains(config.Config.Env, "PATH=/caller/bin:/bin") || !slices.Contains(config.Config.Env, "CHILD_CONFIG=retained") {
			t.Fatalf("caller/child configuration changed: %+v", config.Config)
		}
		imageID = result.ImageID
		manifest = selection.Manifest.Digest
		builds = append(builds, map[string]any{"phase": phase, "imageID": imageID, "manifest": selection.Manifest.Digest, "childRan": childRan, "consumerRan": consumerRan})
	}
	if receipt := os.Getenv("COOPR_NIX_BUNDLE_RECEIPT"); receipt != "" {
		data, err := json.MarshalIndent(map[string]any{"status": "PASS", "bundle": report.Descriptor.Digest, "parent": parent.Descriptor.Digest, "receivedComponents": components, "completeArchive": archive, "scratch": root, "singleCompleteArchiveRestored": true, "registryStoppedBeforeExecution": true, "producerStoreOrComponentArchiveSupplied": false, "nixNamedContextSupplied": false, "literalArgumentAndArgumentClosureExecuted": true, "selectedDeclarationContext": selectedContext, "selectedChild": selectedChild, "sameHostAndUID": true, "nativeArchitecture": runtime.GOARCH, "builds": builds}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(receipt, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
