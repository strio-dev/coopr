package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type configRewriteFixture struct {
	layout      string
	layer       configRewriteLayer
	oldConfig   v1.Descriptor
	oldManifest v1.Descriptor
	manifestRaw map[string]json.RawMessage
	indexRaw    map[string]json.RawMessage
}

type configRewriteLayer struct {
	desc v1.Descriptor
	data []byte
	diff digest.Digest
}

func newConfigRewriteLayer(t *testing.T, name string) configRewriteLayer {
	t.Helper()
	var uncompressed bytes.Buffer
	tarWriter := tar.NewWriter(&uncompressed)
	data := []byte("content")
	if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write(uncompressed.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return configRewriteLayer{
		desc: Descriptor(v1.MediaTypeImageLayerGzip, compressed.Bytes()),
		data: compressed.Bytes(),
		diff: digest.FromBytes(uncompressed.Bytes()),
	}
}

func newConfigRewriteFixture(t *testing.T) configRewriteFixture {
	t.Helper()
	layout := t.TempDir()
	layer := newConfigRewriteLayer(t, "original")
	oldConfigBytes := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["` + layer.diff.String() + `"]},"config":{"Cmd":["old"]}}`)
	oldConfig := Descriptor(v1.MediaTypeImageConfig, oldConfigBytes)
	writeRewriteBlob(t, layout, layer.desc, layer.data)
	writeRewriteBlob(t, layout, oldConfig, oldConfigBytes)

	configDescriptor := map[string]any{
		"mediaType":              oldConfig.MediaType,
		"digest":                 oldConfig.Digest,
		"size":                   oldConfig.Size,
		"data":                   base64.StdEncoding.EncodeToString(oldConfigBytes),
		"urls":                   []string{"https://invalid.example/config"},
		"annotations":            map[string]string{"vendor.config": "preserved"},
		"vendorConfigDescriptor": map[string]bool{"keep": true},
	}
	manifest := map[string]any{
		"schemaVersion":  2,
		"mediaType":      v1.MediaTypeImageManifest,
		"config":         configDescriptor,
		"layers":         []v1.Descriptor{layer.desc},
		"annotations":    map[string]string{"vendor.manifest": "preserved"},
		"vendorManifest": map[string]bool{"keep": true},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	root := Descriptor(v1.MediaTypeImageManifest, manifestBytes)
	writeRewriteBlob(t, layout, root, manifestBytes)

	rootDescriptor := map[string]any{
		"mediaType":            root.MediaType,
		"digest":               root.Digest,
		"size":                 root.Size,
		"data":                 base64.StdEncoding.EncodeToString(manifestBytes),
		"urls":                 []string{"https://invalid.example/manifest"},
		"annotations":          map[string]string{"config.digest": oldConfig.Digest.String(), "vendor.root": "preserved"},
		"vendorRootDescriptor": map[string]bool{"keep": true},
	}
	index := map[string]any{
		"schemaVersion": 2,
		"manifests":     []any{rootDescriptor},
		"annotations":   map[string]string{"vendor.index": "preserved"},
		"vendorIndex":   map[string]bool{"keep": true},
	}
	indexBytes, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout, "index.json"), indexBytes, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0644); err != nil {
		t.Fatal(err)
	}
	manifestRaw, err := decodeLayerObject(manifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	indexRaw, err := decodeLayerObject(indexBytes)
	if err != nil {
		t.Fatal(err)
	}
	return configRewriteFixture{layout: layout, layer: layer, oldConfig: oldConfig, oldManifest: root, manifestRaw: manifestRaw, indexRaw: indexRaw}
}

func TestSetLayoutCreatedAnnotationPreservesRootAnnotations(t *testing.T) {
	fixture := newConfigRewriteFixture(t)
	epoch := int64(123)
	if err := SetLayoutCreatedAnnotation(context.Background(), fixture.layout, &epoch); err != nil {
		t.Fatal(err)
	}
	_, root, err := layoutRoot(fixture.layout)
	if err != nil {
		t.Fatal(err)
	}
	if root.Annotations[v1.AnnotationCreated] != "1970-01-01T00:02:03Z" || root.Annotations["vendor.root"] != "preserved" {
		t.Fatalf("annotations = %#v", root.Annotations)
	}
}

func TestConfigureLayoutCreatedAnnotationRemovesAndDerives(t *testing.T) {
	fixture := newConfigRewriteFixture(t)
	epoch := int64(123)
	if err := SetLayoutCreatedAnnotation(context.Background(), fixture.layout, &epoch); err != nil {
		t.Fatal(err)
	}
	disabled := false
	if err := ConfigureLayoutCreatedAnnotation(context.Background(), fixture.layout, nil, &disabled); err != nil {
		t.Fatal(err)
	}
	_, root, err := layoutRoot(fixture.layout)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := root.Annotations[v1.AnnotationCreated]; present || root.Annotations["vendor.root"] != "preserved" {
		t.Fatalf("annotations after disable = %#v", root.Annotations)
	}
	raw, err := ReadImageConfigLayout(context.Background(), fixture.layout)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	config["created"] = "2000-01-02T03:04:05Z"
	raw, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReplaceImageConfigLayout(context.Background(), fixture.layout, raw); err != nil {
		t.Fatal(err)
	}
	enabled := true
	if err := ConfigureLayoutCreatedAnnotation(context.Background(), fixture.layout, nil, &enabled); err != nil {
		t.Fatal(err)
	}
	_, root, err = layoutRoot(fixture.layout)
	if err != nil {
		t.Fatal(err)
	}
	if root.Annotations[v1.AnnotationCreated] != "2000-01-02T03:04:05Z" {
		t.Fatalf("derived annotation = %#v", root.Annotations)
	}
}

func writeRewriteBlob(t *testing.T, layout string, desc v1.Descriptor, data []byte) {
	t.Helper()
	path := filepath.Join(layout, "blobs", desc.Digest.Algorithm().String(), desc.Digest.Encoded())
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceImageConfigLayoutPreservesManifestAndIndexExtensions(t *testing.T) {
	fixture := newConfigRewriteFixture(t)
	newConfigBytes := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["` + fixture.layer.diff.String() + `"],"vendorRootFS":{"keep":true}},"config":{"Cmd":["new"],"vendorConfig":{"keep":true}},"vendorTop":{"keep":true}}`)
	wantConfig := Descriptor(v1.MediaTypeImageConfig, newConfigBytes)
	emittedConfig, err := ReadImageConfigLayout(context.Background(), fixture.layout)
	if err != nil {
		t.Fatal(err)
	}
	wantEmittedConfig, err := os.ReadFile(filepath.Join(fixture.layout, "blobs", "sha256", fixture.oldConfig.Digest.Encoded()))
	if err != nil || !reflect.DeepEqual(emittedConfig, wantEmittedConfig) {
		t.Fatalf("emitted config = %q, %v", emittedConfig, err)
	}

	configDigest, manifestDigest, err := ReplaceImageConfigLayout(context.Background(), fixture.layout, newConfigBytes)
	if err != nil {
		t.Fatal(err)
	}
	if configDigest != wantConfig.Digest || manifestDigest == "" || manifestDigest == fixture.oldManifest.Digest {
		t.Fatalf("returned digests = %s, %s", configDigest, manifestDigest)
	}
	storedConfig, err := os.ReadFile(filepath.Join(fixture.layout, "blobs", "sha256", configDigest.Encoded()))
	if err != nil || !reflect.DeepEqual(storedConfig, newConfigBytes) {
		t.Fatalf("new config blob = %q, %v", storedConfig, err)
	}

	indexBytes, err := os.ReadFile(filepath.Join(fixture.layout, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	indexRaw, err := decodeLayerObject(indexBytes)
	if err != nil {
		t.Fatal(err)
	}
	if string(indexRaw["vendorIndex"]) != string(fixture.indexRaw["vendorIndex"]) || string(indexRaw["annotations"]) != string(fixture.indexRaw["annotations"]) {
		t.Fatalf("index extensions changed: %s", indexBytes)
	}
	var roots []map[string]json.RawMessage
	if err := json.Unmarshal(indexRaw["manifests"], &roots); err != nil || len(roots) != 1 {
		t.Fatal(err)
	}
	if roots[0]["data"] != nil || roots[0]["urls"] != nil || string(roots[0]["vendorRootDescriptor"]) != `{"keep":true}` {
		t.Fatalf("root descriptor was not safely rewritten: %s", roots[0])
	}
	var rootDescriptors []v1.Descriptor
	if err := json.Unmarshal(indexRaw["manifests"], &rootDescriptors); err != nil {
		t.Fatal(err)
	}
	root := rootDescriptors[0]
	if root.Digest != manifestDigest {
		t.Fatalf("index manifest digest = %s, want %s", root.Digest, manifestDigest)
	}
	var rootAnnotations map[string]string
	if err := json.Unmarshal(roots[0]["annotations"], &rootAnnotations); err != nil || rootAnnotations["config.digest"] != configDigest.String() || rootAnnotations["vendor.root"] != "preserved" {
		t.Fatalf("root annotations = %#v, %v", rootAnnotations, err)
	}

	manifestBytes, err := os.ReadFile(filepath.Join(fixture.layout, "blobs", "sha256", manifestDigest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	if digest.FromBytes(manifestBytes) != manifestDigest {
		t.Fatal("manifest blob digest does not match index")
	}
	manifestRaw, err := decodeLayerObject(manifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"layers", "annotations", "vendorManifest"} {
		if string(manifestRaw[field]) != string(fixture.manifestRaw[field]) {
			t.Fatalf("manifest field %s changed: %s", field, manifestBytes)
		}
	}
	var configRaw map[string]json.RawMessage
	if err := json.Unmarshal(manifestRaw["config"], &configRaw); err != nil {
		t.Fatal(err)
	}
	if configRaw["data"] != nil || configRaw["urls"] != nil || string(configRaw["vendorConfigDescriptor"]) != `{"keep":true}` || string(configRaw["annotations"]) != `{"vendor.config":"preserved"}` {
		t.Fatalf("config descriptor was not safely rewritten: %s", manifestRaw["config"])
	}
	var configDescriptor v1.Descriptor
	if err := json.Unmarshal(manifestRaw["config"], &configDescriptor); err != nil {
		t.Fatal(err)
	}
	if configDescriptor.Digest != configDigest || configDescriptor.Size != int64(len(newConfigBytes)) || configDescriptor.MediaType != v1.MediaTypeImageConfig {
		t.Fatalf("config descriptor = %+v", configDescriptor)
	}
	if _, err := os.Stat(filepath.Join(fixture.layout, "blobs", "sha256", fixture.oldConfig.Digest.Encoded())); err != nil {
		t.Fatalf("immutable old config was removed: %v", err)
	}
}

func TestReplaceImageConfigLayoutRejectsInvalidInputsWithoutChangingIndex(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*testing.T, configRewriteFixture)
		config  func(configRewriteFixture) []byte
		wantErr string
	}{
		{
			name: "diff ID count",
			config: func(configRewriteFixture) []byte {
				return []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
			},
			wantErr: "0 diff IDs for 1 manifest layers",
		},
		{
			name: "diff ID provenance",
			config: func(configRewriteFixture) []byte {
				return []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]}}`)
			},
			wantErr: "diff ID 0 differs from emitted image",
		},
		{
			name:    "invalid config JSON",
			config:  func(configRewriteFixture) []byte { return []byte(`{"rootfs":`) },
			wantErr: "invalid OCI image config",
		},
		{
			name: "corrupt old layer",
			mutate: func(t *testing.T, fixture configRewriteFixture) {
				writeRewriteBlob(t, fixture.layout, fixture.layer.desc, []byte("corrupt"))
			},
			wantErr: "verify OCI layout",
		},
		{
			name: "multiple manifests",
			mutate: func(t *testing.T, fixture configRewriteFixture) {
				data, err := os.ReadFile(filepath.Join(fixture.layout, "index.json"))
				if err != nil {
					t.Fatal(err)
				}
				var index map[string]json.RawMessage
				if err := json.Unmarshal(data, &index); err != nil {
					t.Fatal(err)
				}
				var roots []json.RawMessage
				if err := json.Unmarshal(index["manifests"], &roots); err != nil {
					t.Fatal(err)
				}
				roots = append(roots, roots[0])
				index["manifests"], err = json.Marshal(roots)
				if err != nil {
					t.Fatal(err)
				}
				data, err = json.Marshal(index)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(fixture.layout, "index.json"), data, 0644); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "exactly one root descriptor",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newConfigRewriteFixture(t)
			if test.mutate != nil {
				test.mutate(t, fixture)
			}
			before, err := os.ReadFile(filepath.Join(fixture.layout, "index.json"))
			if err != nil {
				t.Fatal(err)
			}
			config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["` + fixture.layer.diff.String() + `"]}}`)
			if test.config != nil {
				config = test.config(fixture)
			}
			_, _, err = ReplaceImageConfigLayout(context.Background(), fixture.layout, config)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
			after, readErr := os.ReadFile(filepath.Join(fixture.layout, "index.json"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("index changed after rejected rewrite")
			}
		})
	}
}
