package buildah

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/componentstore"
	"github.com/strio-dev/coopr/internal/planner"

	upstreamdefine "go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestAuthoredInlineOnBuildOperationNULBoundary(t *testing.T) {
	for _, kind := range []string{"COPY", "ADD"} {
		trigger := kind + " <<name /out/\nnul\x00exact\rafter\nname"
		builder := &recordingBuilder{}
		if err := OnBuild(trigger).apply(builder, ""); err != nil {
			t.Fatal(err)
		}
		if len(builder.onBuild) != 1 || builder.onBuild[0] != trigger {
			t.Fatalf("stored trigger = %#v", builder.onBuild)
		}
	}
	for _, trigger := range []string{"RUN true\x00", "COPY source\x00 /out/", "COPY <<name /out\x00/\ndata\nname", "COPY --chown=1\x00:2 <<name /out/\ndata\nname"} {
		builder := &recordingBuilder{}
		if err := OnBuild(trigger).apply(builder, ""); err == nil || len(builder.onBuild) != 0 {
			t.Errorf("accepted invalid trigger %q", trigger)
		}
	}
}

func TestAuthoredInlineWorkersCacheAndInvalidation(t *testing.T) {
	for _, kind := range []string{"copy", "add"} {
		t.Run(kind, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.write(t, "same", "ordinary\n")
			f.write(t, ".cooprignore", "second\nraw\nempty\n")
			var firstDigest string
			for _, attempt := range []string{"cold", "warm", "fresh-store", "changed"} {
				body := "first"
				if attempt == "changed" {
					body = "changed"
				}
				doc := parseWorkerDefinition(t, fmt.Sprintf(`from "scratch" as="side" { copy "/side" { file "side" "side"; }; }
from "scratch" as="primary" { arg "value" %q; layer { %s "same" "/out/" chown="12:34" chmod=0o640 link=#true { file "same" "inline\n"; file "second" "${value}\n\n"; file "raw" #"${value}"#; file "empty" ""; file "nul" "nul\u{0}exact"; file "cr" "a\rb\n"; exclude "second" "raw" "empty"; }; }; }
from "primary" { copy "/side" "/side" from="side"; }`, body, kind))
				plan, err := planner.Create(doc, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH})
				if err != nil {
					t.Fatal(err)
				}
				store := f.options.Store
				if attempt == "fresh-store" {
					store = cacheTestStore(filepath.Join(f.root, "fresh"))
				}
				layout := filepath.Join(f.root, attempt)
				result, err := BuildPlanSupervised(f.ctx, plan, SupervisedPlanOptions{Store: store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout}, Jobs: 2, CacheLocalDir: filepath.Join(f.root, "portable"), SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath, Stdout: io.Discard, Stderr: os.Stderr})
				if err != nil {
					t.Fatal(err)
				}
				if attempt == "fresh-store" && result.CacheStats.Hits == 0 {
					t.Fatalf("portable hits = %+v", result.CacheStats)
				}
				manifest, _ := readPlanImage(t, layout)
				if len(manifest.Layers) != 2 {
					t.Fatalf("layers = %d", len(manifest.Layers))
				}
				digest := manifest.Layers[0].Digest.String()
				if attempt == "cold" {
					firstDigest = digest
				} else if attempt == "changed" {
					if digest == firstDigest {
						t.Fatal("changed body reused old layer")
					}
				} else if digest != firstDigest {
					t.Fatal("warm body changed layer")
				}
				blob := filepath.Join(layout, "blobs", "sha256", manifest.Layers[0].Digest.Encoded())
				for name, want := range map[string]string{"same": "ordinary\n", "second": body + "\n\n", "raw": "${value}", "empty": "", "nul": "nul\x00exact", "cr": "a\rb\n"} {
					header := readLayerHeader(t, blob, "out/"+name)
					if header.Mode != 0640 || header.Uid != 12 || header.Gid != 34 {
						t.Fatalf("%s %s metadata = %o %d:%d", attempt, name, header.Mode, header.Uid, header.Gid)
					}
					if got := readLayerFile(t, blob, "out/"+name); got != want {
						t.Fatalf("%s %s = %q, want %q", attempt, name, got, want)
					}
				}
			}
		})
	}
}

func TestAuthoredInlinePublishedPackageAndExtend(t *testing.T) {
	f := newLocalComponentFixture(t)
	doc := parseWorkerDefinition(t, `arg "fixed" "publisher"
arg "dynamic" "default"
arg "literal_shell" "first"
arg "literal_maintainer" "first"
package as="assets" { shell "/bin/${literal_shell}" "-c"; maintainer "${literal_maintainer}"; copy "/fixed" { file "fixed" "${fixed}"; }; }
extend { copy "/fixed" "/fixed" from="assets"; copy "/dynamic" { file "dynamic" "${dynamic}"; }; }`)
	layout := filepath.Join(f.root, "published")
	publication, err := PublishDefinitionSupervised(f.ctx, doc, planner.Options{Mode: planner.Publish, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun", Output: Output{Path: layout}, CacheLocalDir: f.options.CacheLocalDir, SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath, Stdout: io.Discard, Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	source, err := orasoci.NewWithContext(f.ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := componentstore.Put(f.ctx, filepath.Join(f.root, "components"), source, publication.Root, "inline-component"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(f.contextDir); err != nil {
		t.Fatal(err)
	}
	f.contextDir = filepath.Join(f.root, "consumer")
	if err := os.Mkdir(f.contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	f.options.ContextDir = f.contextDir
	for attempt, value := range []string{"caller", "caller", "changed"} {
		_, out := f.build(t, fmt.Sprintf("consume-%d", attempt), fmt.Sprintf(`from "scratch" { component "inline-component" { arg "dynamic" %q; arg "literal_shell" "changed"; arg "literal_maintainer" "changed"; }; }`, value))
		manifest, _ := readPlanImage(t, out)
		if len(manifest.Layers) < 2 {
			t.Fatalf("layers = %d", len(manifest.Layers))
		}
		for index, want := range []struct{ name, data string }{{"fixed", "publisher"}, {"dynamic", value}} {
			blob := filepath.Join(out, "blobs", "sha256", manifest.Layers[len(manifest.Layers)-2+index].Digest.Encoded())
			if got := readLayerFile(t, blob, want.name); got != want.data {
				t.Fatalf("%s = %q", want.name, got)
			}
		}
	}
}

func TestAuthoredInlinePublishedOnBuildNativeAndCoopr(t *testing.T) {
	for _, kind := range []string{"copy", "add"} {
		t.Run(kind, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			for _, name := range []string{`has"quote`, "has'quote", "child-source"} {
				f.write(t, name, "ordinary "+name)
			}
			baseDoc := parseWorkerDefinition(t, fmt.Sprintf(`from "scratch" { copy "/seed" { file "seed" "seed"; }; onbuild { arg "value" "child"; }; onbuild { arg "source" "child-source"; }; onbuild { arg "destination" "child-dest"; }; onbuild { %s "has\"quote" "has'quote" "${source}" "/out-${destination}/" { file "normal" "${value}\n\n"; file "nul" "nul\u{0}exact\n"; file "cr" "a\rb\n"; file "${filename}" "named\n"; file "escapes" "\"${value}\" '\\${value}' C:\\\\literal\n"; file "raw" #"""
${value}

"""#; }; }; }`, kind))
			base, err := BuildDefinitionSupervised(f.ctx, baseDoc, planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun", Output: Output{Path: filepath.Join(f.root, "base"), Format: outputFormatDocker}, SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath, Stdout: io.Discard, Stderr: os.Stderr})
			if err != nil {
				t.Fatal(err)
			}
			const ref = "localhost/coopr-authored-inline:test"
			if err := WithStore(f.options.Store, func(store storage.Store) error { return store.AddNames(base.ImageID, []string{ref}) }); err != nil {
				t.Fatal(err)
			}
			cooprLayout := filepath.Join(f.root, "coopr-child")
			_, err = BuildDefinitionSupervised(f.ctx, parseWorkerDefinition(t, fmt.Sprintf(`from %q {}`, ref)), planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun", Output: Output{Path: cooprLayout}, SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath, Stdout: io.Discard, Stderr: os.Stderr})
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(f.contextDir, "Containerfile")
			if err := os.WriteFile(file, []byte("FROM "+ref+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			nativeLayout := filepath.Join(f.root, "native-child")
			lease, err := acquireStore(f.options.Store)
			if err != nil {
				t.Fatal(err)
			}
			_, _, buildErr := imagebuildah.BuildDockerfiles(f.ctx, lease.store, upstreamdefine.BuildOptions{ContextDirectory: f.contextDir, Output: "oci:" + nativeLayout, Layers: true, PullPolicy: upstreamdefine.PullNever, Isolation: upstreamdefine.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &upstreamdefine.CommonBuildOptions{}, Compression: upstreamdefine.Uncompressed, Out: io.Discard, Err: os.Stderr, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, file)
			closeErr := lease.Close()
			if buildErr != nil || closeErr != nil {
				t.Fatalf("build=%v close=%v", buildErr, closeErr)
			}
			for _, layout := range []string{cooprLayout, nativeLayout} {
				for name, want := range map[string]string{"normal": "child\n\n", "nul": "nul\x00exact\n", "cr": "a\rb\n", "raw": "${value}\n", "${filename}": "named\n", `has"quote`: `ordinary has"quote`, "has'quote": "ordinary has'quote", "child-source": "ordinary child-source", "escapes": "\"child\" '\\${value}' C:\\\\literal\n"} {
					if got := localComponentLastFile(t, layout, "out-child-dest/"+name); got != want {
						t.Fatalf("%s %s = %q", layout, name, got)
					}
				}
			}
		})
	}
}

func TestAuthoredInlineNativeWireBoundary(t *testing.T) {
	f := newLocalComponentFixture(t)
	file := filepath.Join(f.contextDir, "Containerfile")
	f.write(t, "has space", "ordinary")
	f.write(t, `back\slash`, "ordinary")
	for _, variant := range []string{"normal", "empty", "collision", "source-space", "destination-space", "backslash-source", "nul", "embedded-cr", "crlf"} {
		collision := variant == "collision"
		contents := "FROM scratch\nCOPY <<name /out/\npayload\nname"
		if variant == "embedded-cr" {
			contents = "FROM scratch\nCOPY <<name /out/\npayload\rexact\nname"
		}
		if variant == "crlf" {
			contents = "FROM scratch\nCOPY <<name /out/\npayload\r\nname"
		}
		if variant == "nul" {
			contents = "FROM scratch\nCOPY <<name /out/\npayload\x00exact\nname"
		}
		if variant == "backslash-source" {
			contents = "FROM scratch\nCOPY back\\\\slash <<name /out/\npayload\nname"
		}
		if variant == "source-space" {
			contents = "FROM scratch\nCOPY \"has space\" <<name /out/\npayload\nname"
		}
		if variant == "destination-space" {
			contents = "FROM scratch\nCOPY <<name \"/out space/\"\npayload\nname"
		}
		if variant == "empty" {
			contents = "FROM scratch\nCOPY <<name /out/\nname"
		}
		if collision {
			contents = "FROM scratch\nCOPY <<name /out/\nname\npayload\nname\n"
		}
		if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		layout := filepath.Join(f.root, "native-"+variant)
		lease, err := acquireStore(f.options.Store)
		if err != nil {
			t.Fatal(err)
		}
		_, _, buildErr := imagebuildah.BuildDockerfiles(f.ctx, lease.store, upstreamdefine.BuildOptions{ContextDirectory: f.contextDir, Output: "oci:" + layout, Layers: true, PullPolicy: upstreamdefine.PullNever, Isolation: upstreamdefine.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &upstreamdefine.CommonBuildOptions{}, Compression: upstreamdefine.Uncompressed, Out: io.Discard, Err: os.Stderr, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, file)
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
		if collision || strings.HasSuffix(variant, "-space") || variant == "backslash-source" {
			if buildErr == nil {
				t.Fatal("native accepted an unrepresentable source/header")
			}
			t.Logf("native %s boundary: %v", variant, buildErr)
			continue
		}
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		want := "payload\n"
		if variant == "embedded-cr" {
			want = "payload\rexact\n"
		}
		if variant == "nul" {
			want = "payload\x00exact\n"
		}
		if variant == "empty" {
			want = "\n"
		}
		if got := localComponentLastFile(t, layout, "out/name"); got != want {
			t.Fatalf("native nonempty heredoc bytes = %q", got)
		}
	}
}
