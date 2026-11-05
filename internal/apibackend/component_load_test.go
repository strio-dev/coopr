package apibackend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/bale"
	"github.com/strio-dev/coopr/api"
	"github.com/strio-dev/coopr/internal/componentstore"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/planner"
	"oras.land/oras-go/v2/content"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/errdef"
)

func TestComponentArchiveLoadRoundTrip(t *testing.T) {
	rootDir := t.TempDir()
	def, err := definition.Parse(strings.NewReader("extend as=\"base\" {\n env GREETING=\"hello\"\n}\n"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Create(def, planner.Options{Mode: planner.Publish, Platform: "linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(rootDir, "source")
	root, err := oci.WriteComponentLayout(t.Context(), layout, oci.ComponentMetadata{Version: oci.ComponentVersion, Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, Component: *plan.Component}, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := orasoci.NewWithContext(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(rootDir, "component.tar")
	if err = componentstore.WriteArchive(t.Context(), source, root, archive); err != nil {
		t.Fatal(err)
	}
	load := api.ComponentLoad{Archive: archive, ComponentStoreDir: filepath.Join(rootDir, "loaded")}
	request := api.Request{Kind: "component-load", ComponentLoad: &load}
	for range 2 {
		got, err := Execute(t.Context(), request, io.Discard, io.Discard)
		if err != nil || got.Reference != root.Digest.String() || got.ManifestDigest != root.Digest.String() {
			t.Fatalf("load result=%+v err=%v", got, err)
		}
	}
	resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: load.ComponentStoreDir, PullPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.Resolve(t.Context(), root.Digest.String(), v1.Platform{OS: "linux", Architecture: "amd64"}, oci.Component)
	if err != nil || resolved.Root.Digest != root.Digest {
		t.Fatalf("stored digest not reusable: %+v %v", resolved, err)
	}

	exported := filepath.Join(rootDir, "exported.tar")
	published, err := Execute(t.Context(), api.Request{Kind: "publish", Publish: &api.Publish{Artifact: "component", Source: root.Digest.String(), ComponentStoreDir: load.ComponentStoreDir, Destinations: []string{"oci-archive:" + exported}}, Options: api.Options{Store: api.Store{RunRoot: filepath.Join(rootDir, "run"), GraphRoot: filepath.Join(rootDir, "graph")}}}, io.Discard, io.Discard)
	if err != nil || len(published.Destinations) != 1 || published.Destinations[0].Status != "passed" {
		t.Fatalf("loaded component not publishable: %+v %v", published, err)
	}
	restored, err := Execute(t.Context(), api.Request{Kind: "component-load", ComponentLoad: &api.ComponentLoad{Archive: exported, ComponentStoreDir: filepath.Join(rootDir, "restored")}}, io.Discard, io.Discard)
	if err != nil || restored.Reference != root.Digest.String() {
		t.Fatalf("published graph changed: %+v %v", restored, err)
	}
	if err = os.WriteFile(archive, []byte("not an OCI archive"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Execute(t.Context(), request, io.Discard, io.Discard); err == nil {
		t.Fatal("accepted invalid archive")
	}
	if _, err = resolver.Resolve(t.Context(), root.Digest.String(), v1.Platform{OS: "linux", Architecture: "amd64"}, oci.Component); err != nil {
		t.Fatalf("failed import damaged prior graph: %v", err)
	}
}

func TestComponentArchiveLoadValidationAndCancellation(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "component.tar")
	if err := os.WriteFile(archive, nil, 0600); err != nil {
		t.Fatal(err)
	}
	valid := api.Request{Kind: "component-load", ComponentLoad: &api.ComponentLoad{Archive: archive, ComponentStoreDir: filepath.Join(root, "store")}}
	for _, tc := range []struct {
		name   string
		mutate func(*api.Request)
	}{
		{"missing", func(r *api.Request) { r.ComponentLoad = nil }},
		{"relative archive", func(r *api.Request) { r.ComponentLoad.Archive = "component.tar" }},
		{"missing store", func(r *api.Request) { r.ComponentLoad.ComponentStoreDir = "" }},
		{"directory archive", func(r *api.Request) { r.ComponentLoad.Archive = root }},
		{"overlap", func(r *api.Request) { r.ComponentLoad.ComponentStoreDir = root }},
		{"mixed", func(r *api.Request) { r.Build = &api.Build{} }},
		{"outputs", func(r *api.Request) { r.Options.Output.OCI = filepath.Join(root, "output") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := valid
			load := *valid.ComponentLoad
			r.ComponentLoad = &load
			tc.mutate(&r)
			if err := validate(r); err == nil {
				t.Fatal("accepted invalid import")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Execute(ctx, valid, io.Discard, io.Discard)
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != "canceled" {
		t.Fatalf("cancel classification: %v", err)
	}
	if _, err = os.Stat(valid.ComponentLoad.ComponentStoreDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled load created store: %v", err)
	}
}

// These are real Bale destination envelopes, validated by Bale on load, around
// a typed native component. Only the bundle/capture changes between deliveries.
func TestComponentArchiveNamedDeliverySelections(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	def, err := definition.Parse(strings.NewReader("extend as=\"base\" {\n env GREETING=\"hello\"\n}\n"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Create(def, planner.Options{Mode: planner.Publish, Platform: "linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(dir, "source")
	root, err := oci.WriteComponentLayout(ctx, layout, oci.ComponentMetadata{Version: oci.ComponentVersion, Platform: platform, Component: *plan.Component}, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	put := func(mediaType string, raw []byte) v1.Descriptor {
		t.Helper()
		desc := content.NewDescriptorFromBytes(mediaType, raw)
		if err := source.Push(ctx, desc, bytes.NewReader(raw)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			t.Fatal(err)
		}
		return desc
	}
	store := filepath.Join(dir, "loaded")
	var results []api.Result
	for _, name := range []string{"docker.io/library/first", "second:delivery"} {
		record := bale.DestinationRecord{SchemaVersion: 1,
			Bundle:      "delivery.example/bundle@" + digest.FromString(name).String(),
			Source:      "source.example/component@" + root.Digest.String(),
			Destination: "delivery.example/component@" + root.Digest.String(),
			Resolutions: []bale.DestinationResolution{{Capture: digest.FromString(name), Policy: bale.DependencyPolicy{Follow: "auto"}, Outcome: "absent"}},
		}
		raw, err := bale.EncodeDestinationRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		config := put(v1.MediaTypeEmptyJSON, []byte("{}"))
		layer := put(bale.DestinationArtifactType, raw)
		raw, err = json.Marshal(v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: bale.DestinationArtifactType, Config: config, Subject: &root, Layers: []v1.Descriptor{layer}})
		if err != nil {
			t.Fatal(err)
		}
		attachment := put(v1.MediaTypeImageManifest, raw)
		attachment.ArtifactType = bale.DestinationArtifactType
		deliveryDir := filepath.Join(t.TempDir(), "delivery")
		binding := &componentstore.Binding{Repository: "delivery.example/component", Attachments: []v1.Descriptor{attachment}}
		if err := componentstore.PutWithBinding(ctx, deliveryDir, source, root, "archive", binding); err != nil {
			t.Fatal(err)
		}
		deliverySource, deliveryRoot, _, err := componentstore.Resolve(ctx, deliveryDir, "archive")
		if err != nil {
			t.Fatal(err)
		}
		archive := filepath.Join(t.TempDir(), "delivery.tar")
		if err := componentstore.WriteArchive(ctx, deliverySource, deliveryRoot, archive); err != nil {
			t.Fatal(err)
		}
		got, err := Execute(ctx, api.Request{Kind: "component-load", ComponentLoad: &api.ComponentLoad{Archive: archive, ComponentStoreDir: store, Name: name}}, io.Discard, io.Discard)
		canonical, _ := componentstore.NormalizeTag(name)
		if err != nil || got.ManifestDigest != root.Digest.String() || got.Reference != canonical+"@"+root.Digest.String() {
			t.Fatalf("named load: %+v %v", got, err)
		}
		results = append(results, got)
	}
	resolver, err := oci.NewResolver(oci.Options{ComponentStoreDir: store, PullPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	var deliveries []digest.Digest
	for i, result := range results {
		resolved, err := resolver.Resolve(ctx, result.Reference, platform, oci.Component)
		if err != nil || resolved.Delivery == nil {
			t.Fatalf("selection %d: %+v %v", i, resolved, err)
		}
		if resolved.Delivery.Scope.Capture != digest.FromString([]string{"docker.io/library/first", "second:delivery"}[i]) {
			t.Fatalf("selection %d changed delivery: %+v", i, resolved.Delivery)
		}
		deliveries = append(deliveries, resolved.Delivery.Identity())
	}
	if deliveries[0] == deliveries[1] {
		t.Fatal("distinct delivery contexts collapsed")
	}
	if _, err := resolver.Resolve(ctx, root.Digest.String(), platform, oci.Component); err == nil || !strings.Contains(err.Error(), "multiple delivery contexts") {
		t.Fatalf("bare digest ambiguity lost: %v", err)
	}
}

func TestComponentArchiveInvalidNameDoesNotCreateStore(t *testing.T) {
	for _, name := range []string{"bad name", "Uppercase", "http://registry.example/component", "sha256:" + strings.Repeat("a", 64), "component@sha256:" + strings.Repeat("a", 64)} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "archive.tar")
			if err := os.WriteFile(archive, nil, 0600); err != nil {
				t.Fatal(err)
			}
			store := filepath.Join(dir, "store")
			_, err := Execute(t.Context(), api.Request{Kind: "component-load", ComponentLoad: &api.ComponentLoad{Archive: archive, ComponentStoreDir: store, Name: name}}, io.Discard, io.Discard)
			var failure *api.Error
			if !errors.As(err, &failure) || failure.Code != "invalid_request" || !strings.Contains(err.Error(), "name") {
				t.Fatalf("invalid name classification: %v", err)
			}
			if _, err := os.Stat(store); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid name created store: %v", err)
			}
		})
	}
}
