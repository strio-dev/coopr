package imagecatalog

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestCatalogPreservesPlatformSelectionsAndOldDigestAcrossTagRefresh(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "images")
	ref := "registry.example/team/base:latest"
	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := v1.Platform{OS: "linux", Architecture: "arm64"}
	root := descriptor(v1.MediaTypeImageIndex, "old-index")
	amd := testSelection(root, descriptor(v1.MediaTypeImageManifest, "old-amd"), `{"os":"linux","architecture":"amd64"}`)
	arm := testSelection(root, descriptor(v1.MediaTypeImageManifest, "old-arm"), `{"os":"linux","architecture":"arm64"}`)
	if err := Commit(ctx, dir, ref, amd64, amd); err != nil {
		t.Fatal(err)
	}
	if err := Commit(ctx, dir, ref, arm64, arm); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		platform v1.Platform
		want     Selection
	}{{amd64, amd}, {arm64, arm}} {
		got, found, err := Lookup(ctx, dir, ref, item.platform)
		if err != nil || !found || got.ImageID != item.want.ImageID || got.Manifest.Digest != item.want.Manifest.Digest {
			t.Fatalf("lookup %s = %+v, found %t, err %v", item.platform.Architecture, got, found, err)
		}
	}
	newRoot := descriptor(v1.MediaTypeImageIndex, "new-index")
	newAMD := testSelection(newRoot, descriptor(v1.MediaTypeImageManifest, "new-amd"), `{"os":"linux","architecture":"amd64"}`)
	if err := Commit(ctx, dir, ref, amd64, newAMD); err != nil {
		t.Fatal(err)
	}
	if got, found, err := Lookup(ctx, dir, ref, amd64); err != nil || !found || got.Root.Digest != newRoot.Digest {
		t.Fatalf("ref did not refresh: %+v, found %t, err %v", got, found, err)
	}
	if got, found, err := Lookup(ctx, dir, root.Digest.String(), arm64); err != nil || !found || got.Manifest.Digest != arm.Manifest.Digest {
		t.Fatalf("old root digest lost arm64: %+v, found %t, err %v", got, found, err)
	}
	if got, found, err := Lookup(ctx, dir, amd.Manifest.Digest.String(), amd64); err != nil || !found || got.ImageID != amd.ImageID {
		t.Fatalf("selected manifest digest lost: %+v, found %t, err %v", got, found, err)
	}
}

func TestAvailablePlatformsRetainsPartialIndexesAndRestrictedAliases(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	root := descriptor(v1.MediaTypeImageIndex, "partial-index")
	ref := "registry.example/team/base:latest"
	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := v1.Platform{OS: "linux", Architecture: "arm64"}
	amd := testSelection(root, descriptor(v1.MediaTypeImageManifest, "partial-amd"), `{"os":"linux","architecture":"amd64"}`)
	arm := testSelection(root, descriptor(v1.MediaTypeImageManifest, "partial-arm"), `{"os":"linux","architecture":"arm64"}`)
	if err := Commit(ctx, dir, ref, amd64, amd); err != nil {
		t.Fatal(err)
	}
	if got, err := AvailablePlatforms(ctx, dir, ref); err != nil || len(got) != 1 || got[0].Architecture != "amd64" {
		t.Fatalf("partial index platforms = %+v, %v", got, err)
	}
	if err := Commit(ctx, dir, ref, arm64, arm); err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{ref, root.Digest.String()} {
		if got, err := AvailablePlatforms(ctx, dir, selector); err != nil || len(got) != 2 || got[0].Architecture != "amd64" || got[1].Architecture != "arm64" {
			t.Fatalf("index platforms for %s = %+v, %v", selector, got, err)
		}
	}
	const leaf = "registry.example/team/base@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := CommitSelected(ctx, dir, leaf, amd64, amd); err != nil {
		t.Fatal(err)
	}
	if got, err := AvailablePlatforms(ctx, dir, leaf); err != nil || len(got) != 1 || got[0].Architecture != "amd64" {
		t.Fatalf("restricted leaf platforms = %+v, %v", got, err)
	}
	if got, err := AvailablePlatforms(ctx, dir, "absent:latest"); err != nil || len(got) != 0 {
		t.Fatalf("absent platforms = %+v, %v", got, err)
	}
}

func TestCatalogMaintenanceRemovesNamesBeforeMissingRoots(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "images")
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	root := descriptor(v1.MediaTypeImageManifest, "maintenance")
	selection := testSelection(root, root, `{"os":"linux","architecture":"amd64"}`)
	if err := Commit(ctx, dir, "example:latest", platform, selection); err != nil {
		t.Fatal(err)
	}
	entries, err := List(ctx, dir)
	if err != nil || len(entries) != 1 || entries[0].Reference != "example:latest" {
		t.Fatalf("catalog list = %+v, %v", entries, err)
	}
	ids, err := NamedImageIDs(ctx, dir)
	if err != nil || !ids[selection.ImageID] {
		t.Fatalf("named IDs = %+v, %v", ids, err)
	}
	if removed, err := RemoveReference(ctx, dir, "example:latest"); err != nil || !removed {
		t.Fatalf("remove reference = %t, %v", removed, err)
	}
	if removed, err := PruneMissing(ctx, dir, nil, true); err != nil || removed == 0 {
		t.Fatalf("dry-run roots = %d, %v", removed, err)
	}
	if _, found, err := Lookup(ctx, dir, root.Digest.String(), platform); err != nil || !found {
		t.Fatalf("dry-run removed root: found=%t err=%v", found, err)
	}
	if removed, err := PruneMissing(ctx, dir, nil, false); err != nil || removed == 0 {
		t.Fatalf("prune roots = %d, %v", removed, err)
	}
	if _, found, err := Lookup(ctx, dir, root.Digest.String(), platform); err != nil || found {
		t.Fatalf("missing root remains: found=%t err=%v", found, err)
	}
}

func TestCatalogPruneRemovesIncompleteIndexAndPreservesLiveManifestAlias(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "images")
	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := v1.Platform{OS: "linux", Architecture: "arm64"}
	amdManifest := descriptor(v1.MediaTypeImageManifest, "prune-index-amd")
	armManifest := descriptor(v1.MediaTypeImageManifest, "prune-index-arm")
	amdManifest.Platform = &amd64
	armManifest.Platform = &arm64
	indexData, root := testIndex(t, v1.MediaTypeImageIndex, amdManifest, armManifest)
	amd := testSelection(root, amdManifest, `{"os":"linux","architecture":"amd64"}`)
	arm := testSelection(root, armManifest, `{"os":"linux","architecture":"arm64"}`)
	if err := CommitIndex(ctx, dir, "", root, indexData, map[string]Selection{"linux/amd64": amd, "linux/arm64": arm}); err != nil {
		t.Fatal(err)
	}
	removed, err := PruneMissing(ctx, dir, map[string]bool{amd.ImageID: true}, false)
	if err != nil || removed != 2 {
		t.Fatalf("partial index prune = %d, %v; want index and dead manifest", removed, err)
	}
	if _, _, _, found, err := LookupIndex(ctx, dir, root.Digest.String()); err != nil || found {
		t.Fatalf("incomplete index remains: found=%t err=%v", found, err)
	}
	if got, platform, found, err := LookupManifest(ctx, dir, amdManifest.Digest.String()); err != nil || !found || got.ImageID != amd.ImageID || platform.Architecture != "amd64" {
		t.Fatalf("live manifest alias = %+v on %+v, found=%t err=%v", got, platform, found, err)
	}
	if _, _, found, err := LookupManifest(ctx, dir, armManifest.Digest.String()); err != nil || found {
		t.Fatalf("dead manifest alias remains: found=%t err=%v", found, err)
	}
}

func TestCatalogRejectsConfigurationThatDoesNotIdentifyStoredImage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	root := descriptor(v1.MediaTypeImageManifest, "manifest")
	selection := testSelection(root, root, `{"os":"linux","architecture":"amd64"}`)
	selection.ImageID = digest.FromString("wrong").Encoded()
	if err := Commit(context.Background(), dir, "base:dev", v1.Platform{OS: "linux", Architecture: "amd64"}, selection); err == nil {
		t.Fatal("committed a config with a different storage image ID")
	}
}

func TestCatalogPreservesAllPlatformsForSelectedManifestDigest(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "images")
	root := descriptor(v1.MediaTypeImageIndex, "index")
	manifest := descriptor(v1.MediaTypeImageManifest, "shared-manifest")
	selection := testSelection(root, manifest, `{"os":"linux","architecture":"amd64"}`)
	platforms := []v1.Platform{
		{OS: "linux", Architecture: "amd64"},
		{OS: "linux", Architecture: "amd64", Variant: "v3"},
	}
	for _, platform := range platforms {
		if err := Commit(ctx, dir, "base:dev", platform, selection); err != nil {
			t.Fatal(err)
		}
	}
	for _, platform := range platforms {
		got, found, err := Lookup(ctx, dir, manifest.Digest.String(), platform)
		if err != nil || !found || got.ImageID != selection.ImageID {
			t.Fatalf("selected manifest lookup for %+v = %+v, found %t, err %v", platform, got, found, err)
		}
	}
}

func TestCommitSelectedRestrictsReferenceWithoutHidingDigestPlatforms(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "images")
	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := v1.Platform{OS: "linux", Architecture: "arm64"}
	manifest := descriptor(v1.MediaTypeImageManifest, "shared-manifest")
	selection := testSelection(manifest, manifest, `{"os":"linux","architecture":"amd64"}`)
	if err := Commit(ctx, dir, "native:latest", amd64, selection); err != nil {
		t.Fatal(err)
	}
	if err := Commit(ctx, dir, "foreign:latest", arm64, selection); err != nil {
		t.Fatal(err)
	}
	if err := CommitSelected(ctx, dir, "foreign-copy:latest", arm64, selection); err != nil {
		t.Fatal(err)
	}
	if _, found, err := Lookup(ctx, dir, "foreign-copy:latest", amd64); err != nil || found {
		t.Fatalf("selected reference exposed amd64: found=%t, err=%v", found, err)
	}
	if got, found, err := Lookup(ctx, dir, "foreign-copy:latest", arm64); err != nil || !found || got.Manifest.Digest != manifest.Digest {
		t.Fatalf("selected reference arm64 = %+v, found=%t, err=%v", got, found, err)
	}
	if got, platform, found, err := LookupSole(ctx, dir, "foreign-copy:latest"); err != nil || !found || platform.Architecture != "arm64" || got.Manifest.Digest != manifest.Digest {
		t.Fatalf("selected reference sole lookup = %+v on %+v, found=%t, err=%v", got, platform, found, err)
	}
	if _, found, err := Lookup(ctx, dir, manifest.Digest.String(), amd64); err != nil || !found {
		t.Fatalf("immutable digest lost amd64: found=%t, err=%v", found, err)
	}
}

func TestLookupManifestIgnoresPlatformButRejectsIndexDigest(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "images")
	arm64 := v1.Platform{OS: "linux", Architecture: "arm64"}
	root := descriptor(v1.MediaTypeImageIndex, "arm-index")
	manifest := descriptor(v1.MediaTypeImageManifest, "arm-manifest")
	selection := testSelection(root, manifest, `{"os":"linux","architecture":"arm64"}`)
	if err := Commit(ctx, dir, "arm:latest", arm64, selection); err != nil {
		t.Fatal(err)
	}
	got, gotPlatform, found, err := LookupManifest(ctx, dir, manifest.Digest.String())
	if err != nil || !found || got.Manifest.Digest != manifest.Digest || got.ImageID != selection.ImageID || gotPlatform.Architecture != "arm64" {
		t.Fatalf("exact manifest lookup = %+v on %+v, found=%t, err=%v", got, gotPlatform, found, err)
	}
	if got, gotPlatform, found, err := LookupManifest(ctx, dir, root.Digest.String()); err != nil || found {
		t.Fatalf("index digest lookup = %+v on %+v, found=%t, err=%v", got, gotPlatform, found, err)
	}
}

func TestCommitIndexStoresExactBytesAndPlatformSelections(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "images")
	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	amdManifest := descriptor(v1.MediaTypeImageManifest, "index-amd")
	armManifest := descriptor(v1.MediaTypeImageManifest, "index-arm")
	amdManifest.Platform = &amd64
	armManifest.Platform = &arm64
	indexData, root := testIndex(t, v1.MediaTypeImageIndex, amdManifest, armManifest)
	selections := map[string]Selection{
		"linux/amd64": testSelection(root, amdManifest, `{"os":"linux","architecture":"amd64"}`),
		"linux/arm64": testSelection(root, armManifest, `{"os":"linux","architecture":"arm64","variant":"v8"}`),
	}
	if err := CommitIndex(ctx, dir, "app:latest", root, indexData, selections); err != nil {
		t.Fatal(err)
	}
	gotRoot, gotData, gotSelections, found, err := LookupIndex(ctx, dir, "app:latest")
	if err != nil || !found || gotRoot.Digest != root.Digest || string(gotData) != string(indexData) || len(gotSelections) != 2 {
		t.Fatalf("lookup index = %+v, %q, %+v, found=%t, err=%v", gotRoot, gotData, gotSelections, found, err)
	}
	if got, found, err := Lookup(ctx, dir, amdManifest.Digest.String(), amd64); err != nil || !found || got.Root.Digest != amdManifest.Digest {
		t.Fatalf("manifest alias = %+v, found=%t, err=%v", got, found, err)
	}
}

func TestCommitIndexAcceptsDockerManifestList(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "images")
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	manifest := descriptor(dockerManifestMediaType, "docker-manifest")
	manifest.Platform = &platform
	indexData, root := testIndex(t, dockerIndexMediaType, manifest)
	selection := testSelection(root, manifest, `{"os":"linux","architecture":"amd64"}`)
	if err := CommitIndex(ctx, dir, "docker:latest", root, indexData, map[string]Selection{"linux/amd64": selection}); err != nil {
		t.Fatal(err)
	}
	gotRoot, gotData, _, found, err := LookupIndex(ctx, dir, "docker:latest")
	if err != nil || !found || gotRoot.MediaType != dockerIndexMediaType || string(gotData) != string(indexData) {
		t.Fatalf("docker index = %+v, %q, found=%t, err=%v", gotRoot, gotData, found, err)
	}
}

func TestCommitIndexRetagsAtomicallyAndPreservesOldIndex(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "images")
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	commit := func(seed string) (v1.Descriptor, Selection) {
		manifest := descriptor(v1.MediaTypeImageManifest, seed+"-manifest")
		manifest.Platform = &platform
		data, root := testIndex(t, v1.MediaTypeImageIndex, manifest)
		selection := testSelection(root, manifest, `{"os":"linux","architecture":"amd64","seed":"`+seed+`"}`)
		if err := CommitIndex(ctx, dir, "app:latest", root, data, map[string]Selection{"linux/amd64": selection}); err != nil {
			t.Fatal(err)
		}
		return root, selection
	}
	oldRoot, _ := commit("old")
	newRoot, newSelection := commit("new")
	if _, _, got, found, err := LookupIndex(ctx, dir, "app:latest"); err != nil || !found || got["linux/amd64"].Manifest.Digest != newSelection.Manifest.Digest {
		t.Fatalf("retagged index = %+v, found=%t, err=%v", got, found, err)
	}
	if _, _, _, found, err := LookupIndex(ctx, dir, oldRoot.Digest.String()); err != nil || !found {
		t.Fatalf("old immutable index found=%t, err=%v", found, err)
	}
	if oldRoot.Digest == newRoot.Digest {
		t.Fatal("test indexes unexpectedly have the same digest")
	}
}

func TestCommitIndexRejectsPartialDuplicateAndMismatchedRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	ctx := context.Background()
	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := v1.Platform{OS: "linux", Architecture: "arm64"}
	amdManifest := descriptor(v1.MediaTypeImageManifest, "invalid-amd")
	armManifest := descriptor(v1.MediaTypeImageManifest, "invalid-arm")
	amdManifest.Platform = &amd64
	armManifest.Platform = &arm64
	data, root := testIndex(t, v1.MediaTypeImageIndex, amdManifest, armManifest)
	amd := testSelection(root, amdManifest, `{"os":"linux","architecture":"amd64"}`)
	arm := testSelection(root, armManifest, `{"os":"linux","architecture":"arm64"}`)

	tests := []struct {
		name       string
		data       []byte
		root       v1.Descriptor
		selections map[string]Selection
	}{
		{name: "partial", data: data, root: root, selections: map[string]Selection{"linux/amd64": amd}},
		{name: "mismatched manifest", data: data, root: root, selections: map[string]Selection{"linux/amd64": arm, "linux/arm64": amd}},
	}
	duplicateData, duplicateRoot := testIndex(t, v1.MediaTypeImageIndex, amdManifest, amdManifest)
	tests = append(tests, struct {
		name       string
		data       []byte
		root       v1.Descriptor
		selections map[string]Selection
	}{name: "duplicate platform", data: duplicateData, root: duplicateRoot, selections: map[string]Selection{"linux/amd64": testSelection(duplicateRoot, amdManifest, `{"os":"linux","architecture":"amd64"}`)}})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := CommitIndex(ctx, dir, "bad:latest", test.root, test.data, test.selections); err == nil {
				t.Fatal("committed invalid index")
			}
			if _, _, _, found, err := LookupIndex(ctx, dir, "bad:latest"); err != nil || found {
				t.Fatalf("invalid commit changed catalog: found=%t, err=%v", found, err)
			}
		})
	}
}

func TestLookupIndexIgnoresExternallyImportedPartialIndex(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "images")
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	root := descriptor(v1.MediaTypeImageIndex, "external-index")
	selection := testSelection(root, descriptor(v1.MediaTypeImageManifest, "external-manifest"), `{"os":"linux","architecture":"amd64"}`)
	if err := Commit(ctx, dir, "external:latest", platform, selection); err != nil {
		t.Fatal(err)
	}
	if _, _, _, found, err := LookupIndex(ctx, dir, "external:latest"); err != nil || found {
		t.Fatalf("partial imported index found=%t, err=%v", found, err)
	}
}

func testIndex(t *testing.T, mediaType string, manifests ...v1.Descriptor) ([]byte, v1.Descriptor) {
	t.Helper()
	data, err := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: mediaType, Manifests: manifests})
	if err != nil {
		t.Fatal(err)
	}
	return data, v1.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
}

func descriptor(mediaType, data string) v1.Descriptor {
	return v1.Descriptor{MediaType: mediaType, Digest: digest.FromString(data), Size: int64(len(data))}
}

func testSelection(root, selected v1.Descriptor, config string) Selection {
	return Selection{Root: root, Manifest: selected, ImageID: digest.FromBytes([]byte(config)).Encoded(), ConfigData: []byte(config)}
}
