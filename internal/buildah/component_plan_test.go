package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"coopr/internal/componentstore"
	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestResolveComponentPlanBindsVerifiedPackages(t *testing.T) {
	ctx := context.Background()
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	resolver, root, packageData := localComponentResolver(t, ctx, platform)

	resolved, err := ResolveComponentPlan(ctx, ComponentPlanRequest{
		Resolver: resolver, Reference: "local:tool", Parameters: map[string]string{"channel": "preview"}, Platform: platform,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Identity != root.Digest || resolved.Artifact.Selected.Digest != root.Digest {
		t.Fatalf("component identity = %s selected=%s, want %s", resolved.Identity, resolved.Artifact.Selected.Digest, root.Digest)
	}
	if resolved.Plan.Mode != planner.Invoke || resolved.Plan.Platform != "linux/amd64" || len(resolved.Plan.Stages) != 2 {
		t.Fatalf("invocation plan = %+v", resolved.Plan)
	}
	if resolved.Plan.Stages[0].Kind != "package-input" || resolved.Plan.Stages[0].ID != "0" {
		t.Fatalf("package-input stage = %+v", resolved.Plan.Stages[0])
	}
	foundChannel := false
	for _, operation := range resolved.Plan.Stages[1].Operations {
		if operation.Name == "env" && operation.Properties["CHANNEL"] == "preview" {
			foundChannel = true
		}
	}
	if !foundChannel {
		t.Fatalf("instantiated parameter missing from operations: %+v", resolved.Plan.Stages[1].Operations)
	}
	input, ok := resolved.PackageInputs["0"]
	if !ok || input.Stage != "pkg" || input.Descriptor.Digest != digest.FromBytes(packageData) {
		t.Fatalf("package input = %+v", input)
	}

	download := filepath.Join(t.TempDir(), "package.tar")
	if err := resolver.Download(ctx, resolved.Artifact, input.Descriptor, download); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(download)
	if err != nil || !bytes.Equal(got, packageData) {
		t.Fatalf("downloaded package = %q, %v", got, err)
	}

	input.Config[0] ^= 0xff
	if bytes.Equal(input.Config, resolved.Artifact.Component.Packages[0].Config) {
		t.Fatal("package input aliases verified metadata")
	}
}

func TestResolveComponentPlanRejectsInvalidRequestsAndParameters(t *testing.T) {
	ctx := context.Background()
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	resolver, _, _ := localComponentResolver(t, ctx, platform)
	tests := []struct {
		name    string
		request ComponentPlanRequest
		message string
	}{
		{name: "resolver", request: ComponentPlanRequest{Reference: "local:tool", Platform: platform}, message: "resolver is required"},
		{name: "reference", request: ComponentPlanRequest{Resolver: resolver, Platform: platform}, message: "reference is required"},
		{name: "platform", request: ComponentPlanRequest{Resolver: resolver, Reference: "local:tool", Platform: v1.Platform{OS: "windows", Architecture: "amd64"}}, message: "Linux"},
		{name: "platform features", request: ComponentPlanRequest{Resolver: resolver, Reference: "local:tool", Platform: v1.Platform{OS: "linux", Architecture: "amd64", OSFeatures: []string{"feature"}}}, message: "feature platform fields"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveComponentPlan(ctx, test.request)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error = %v, want %q", err, test.message)
			}
		})
	}
	for _, parameters := range []map[string]string{nil, {"typo": "value"}} {
		resolved, err := ResolveComponentPlan(ctx, ComponentPlanRequest{
			Resolver: resolver, Reference: "local:tool", Platform: platform, Parameters: parameters,
		})
		if err != nil {
			t.Fatalf("unset or unused component parameter: %v", err)
		}
		foundEmptyChannel := false
		for _, operation := range resolved.Plan.Stages[1].Operations {
			if operation.Name == "env" && operation.Properties["CHANNEL"] == "" {
				foundEmptyChannel = true
			}
		}
		if !foundEmptyChannel {
			t.Fatalf("unset parameter did not expand to empty: %+v", resolved.Plan.Stages[1].Operations)
		}
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ResolveComponentPlan(canceled, ComponentPlanRequest{Resolver: resolver, Reference: "local:tool", Platform: platform}); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("canceled context error = %v", err)
	}
}

func TestResolveComponentPlanSelectsExternalCopyImage(t *testing.T) {
	ctx := context.Background()
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	const reference = "registry.example/toolbox:latest"
	resolver, _, _ := localComponentResolverWithDefinition(t, ctx, platform, `package as="pkg"
extend
copy "/artifact" "/artifact" from="pkg"
copy "/bin/tool" "/tool" from="registry.example/toolbox:latest"
`)
	calls := 0
	resolved, err := ResolveComponentPlan(ctx, ComponentPlanRequest{
		Resolver: resolver, Reference: "local:tool", Platform: platform,
		ResolveBase: func(_ context.Context, got string, gotPlatform v1.Platform) (ResolvedImageSource, error) {
			calls++
			if got != reference || gotPlatform.OS != platform.OS || gotPlatform.Architecture != platform.Architecture {
				t.Fatalf("resolve %q for %+v", got, gotPlatform)
			}
			return ResolvedImageSource{ImageID: "immutable-toolbox", ConfigData: []byte(`{"architecture":"amd64","os":"linux"}`)}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	key := ResolvedBaseKey{Reference: reference, Platform: "linux/amd64"}
	if calls != 1 || resolved.SelectedBases[key].ImageID != "immutable-toolbox" {
		t.Fatalf("calls=%d selected=%+v", calls, resolved.SelectedBases)
	}
	if len(resolved.Plan.Inputs) != 2 || resolved.Plan.Inputs[1] != (planner.Input{Kind: "image", Reference: reference}) {
		t.Fatalf("component inputs = %+v", resolved.Plan.Inputs)
	}
}

func localComponentResolver(t *testing.T, ctx context.Context, platform v1.Platform) (*oci.Resolver, v1.Descriptor, []byte) {
	t.Helper()
	return localComponentResolverWithDefinition(t, ctx, platform, `package as="pkg"
extend as="base"
arg "channel"
env CHANNEL="${channel}"
copy "/artifact" "/artifact" from="pkg"
`)
}

func localComponentResolverWithDefinition(t *testing.T, ctx context.Context, platform v1.Platform, definitionSource string) (*oci.Resolver, v1.Descriptor, []byte) {
	t.Helper()
	def, err := definition.Parse(strings.NewReader(definitionSource))
	if err != nil {
		t.Fatal(err)
	}
	publication, err := planner.Create(def, planner.Options{Mode: planner.Publish, Platform: platform.OS + "/" + platform.Architecture})
	if err != nil {
		t.Fatal(err)
	}
	packageData := packageImportTar(t)
	packageDescriptor := oci.Descriptor(oci.ComponentPackageType, packageData)
	packageConfig, err := json.Marshal(v1.Image{
		Platform: platform, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{packageDescriptor.Digest}},
	})
	if err != nil {
		t.Fatal(err)
	}
	meta := oci.ComponentMetadata{
		Version: oci.ComponentVersion, Platform: platform, Component: *publication.Component,
		Packages: []oci.Package{{Stage: "pkg", Descriptor: packageDescriptor, Config: packageConfig}},
	}
	rootDir := t.TempDir()
	packagePath := filepath.Join(rootDir, "package.tar")
	if err := os.WriteFile(packagePath, packageData, 0o600); err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(rootDir, "layout")
	root, err := oci.WriteComponentLayout(ctx, layout, meta, map[string]string{"pkg": packagePath})
	if err != nil {
		t.Fatal(err)
	}
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	componentDir := filepath.Join(rootDir, "components")
	if err := componentstore.Put(ctx, componentDir, source, root, "tool"); err != nil {
		t.Fatal(err)
	}
	resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: componentDir})
	if err != nil {
		t.Fatal(err)
	}
	return resolver, root, packageData
}
