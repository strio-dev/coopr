package apibackend

import (
	"context"
	"errors"
	"github.com/strio-dev/coopr"
	"github.com/strio-dev/coopr/api"
	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/transfer"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func request(t *testing.T) api.Request {
	t.Helper()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	return api.Request{Kind: "execute", Program: &api.Program{States: []api.State{{ID: "result", From: api.Source{Image: "scratch"}}}, Output: "result"}, Options: api.Options{ContextDir: contextDir, Store: api.Store{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph")}, Output: api.Output{OCI: filepath.Join(root, "output")}}}
}

func TestSelectedFileExportsValidateBeforeNativeMutation(t *testing.T) {
	for _, scenario := range []string{"valid", "relative-source", "traversal", "context", "store", "output", "overlapping", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			r := request(t)
			root := filepath.Dir(r.Options.ContextDir)
			r.Options.Output.Exports = []api.FileExport{{Source: "/report", Destination: filepath.Join(root, "report")}}
			switch scenario {
			case "relative-source":
				r.Options.Output.Exports[0].Source = "report"
			case "traversal":
				r.Options.Output.Exports[0].Source = "/a/../report"
			case "context":
				r.Options.Output.Exports[0].Destination = filepath.Join(r.Options.ContextDir, "report")
			case "store":
				r.Options.Output.Exports[0].Destination = filepath.Join(r.Options.Store.GraphRoot, "report")
			case "output":
				r.Options.Output.Exports[0].Destination = filepath.Join(r.Options.Output.OCI, "report")
			case "overlapping":
				r.Options.Output.Exports = append(r.Options.Output.Exports, api.FileExport{Source: "/second", Destination: filepath.Join(root, "report", "second")})
			case "symlink":
				alias := filepath.Join(root, "alias")
				if err := os.Symlink(r.Options.ContextDir, alias); err != nil {
					t.Fatal(err)
				}
				r.Options.Output.Exports[0].Destination = filepath.Join(alias, "report")
			}
			err := validate(r)
			if (err == nil) != (scenario == "valid") {
				t.Fatalf("selected export admission %s: %v", scenario, err)
			}
			if _, err := os.Lstat(r.Options.Store.GraphRoot); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("validation created native storage: %v", err)
			}
		})
	}
}
func TestLiteralProgram(t *testing.T) {
	args := []string{"sh", "-c", `${literal} 'quotes' spaces\n`}
	input := api.Program{Output: "out", States: []api.State{{ID: "out", From: api.Source{Image: "scratch"}, Operations: []api.Operation{{Kind: "env", Name: "VALUE", Value: "${literal}"}, {Kind: "run", Args: args}, {Kind: "copy", Sources: []string{"a b"}, Destination: "/a b"}}}}}
	got, err := program(input)
	if err != nil {
		t.Fatal(err)
	}
	if got.States[0].Operations[0].(coopr.Env).Value != "${literal}" || !reflect.DeepEqual(got.States[0].Operations[1].(coopr.Run).Args, args) {
		t.Fatal("literal operations changed")
	}
	input.States[0].Operations[0].Args = []string{"unexpected"}
	if _, err := program(input); err == nil {
		t.Fatal("accepted mixed operation fields")
	}
	input.States[0].Operations = []api.Operation{{Kind: "unknown"}}
	if _, err := program(input); err == nil {
		t.Fatal("accepted unknown operation")
	}
}
func TestValidationBeforeExecution(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*api.Request)
	}{
		{"negative jobs", func(r *api.Request) { r.Options.Jobs = -1 }},
		{"two payloads", func(r *api.Request) { r.Build = &api.Build{} }},
		{"relative store", func(r *api.Request) { r.Options.Store.GraphRoot = "relative" }},
		{"overlapping stores", func(r *api.Request) { r.Options.Store.GraphRoot = r.Options.Store.RunRoot }},
		{"output in context", func(r *api.Request) { r.Options.Output.OCI = filepath.Join(r.Options.ContextDir, "out") }},
		{"unknown kind", func(r *api.Request) { r.Kind = "other" }},
		{"invalid publish", func(r *api.Request) { r.Kind = "publish"; r.Program = nil; r.Publish = &api.Publish{Artifact: "bad"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request(t)
			tc.edit(&r)
			_, err := Execute(context.Background(), r, nil, nil)
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != "invalid_request" {
				t.Fatalf("error=%v", err)
			}
		})
	}
	r := request(t)
	if err := os.Mkdir(r.Options.Output.OCI, 0700); err != nil {
		t.Fatal(err)
	}
	if err := validate(r); err == nil {
		t.Fatal("existing output accepted")
	}
}

func TestRegistryCacheValidationBeforeAnyBuildMutation(t *testing.T) {
	for _, kind := range []string{"execute", "build", "component"} {
		for _, direction := range []string{"from", "to"} {
			t.Run(kind+"/"+direction, func(t *testing.T) {
				r := request(t)
				if kind != "execute" {
					r.Kind, r.Program = kind, nil
					file := filepath.Join(r.Options.ContextDir, "build.coopr")
					if err := os.WriteFile(file, []byte("from scratch {}"), 0600); err != nil {
						t.Fatal(err)
					}
					r.Build = &api.Build{Files: []string{file}}
				}
				r.Options.Cache.LocalDir = filepath.Join(filepath.Dir(r.Options.ContextDir), "cache")
				if direction == "from" {
					r.Options.Cache.RegistryFrom = []string{"registry.example/cache:tag"}
				} else {
					r.Options.Cache.RegistryTo = []string{"registry.example/cache:tag"}
				}
				_, err := Execute(context.Background(), r, nil, nil)
				var apiErr *api.Error
				if !errors.As(err, &apiErr) || apiErr.Code != "invalid_request" || !strings.Contains(apiErr.Message, "registry cache") {
					t.Fatalf("invalid cache error: %v", err)
				}
				for _, resource := range []string{r.Options.Store.RunRoot, r.Options.Store.GraphRoot, r.Options.Cache.LocalDir, r.Options.Output.OCI} {
					if _, err := os.Lstat(resource); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("invalid cache mutated %s: %v", resource, err)
					}
				}
			})
		}
	}
}
func TestCleanupFailureClassification(t *testing.T) {
	err := executionError(errors.Join(context.Canceled, buildah.ErrWorkerWaitIncomplete))
	var typed *api.Error
	if !errors.As(err, &typed) || typed.Code != "cleanup_failed" {
		t.Fatalf("lost cleanup failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Execute(ctx, request(t), nil, nil)
	if !errors.As(err, &typed) || typed.Code != "canceled" {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestSymlinksCannotConcealOverlap(t *testing.T) {
	r := request(t)
	link := filepath.Join(filepath.Dir(r.Options.ContextDir), "store-link")
	if err := os.Symlink(r.Options.ContextDir, link); err != nil {
		t.Fatal(err)
	}
	r.Options.Store.GraphRoot = link
	if err := validate(r); err == nil {
		t.Fatal("accepted store resolving inside context")
	}
	r = request(t)
	if err := os.Mkdir(r.Options.Store.GraphRoot, 0700); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(filepath.Dir(r.Options.ContextDir), "output-link")
	if err := os.Symlink(r.Options.Store.GraphRoot, link); err != nil {
		t.Fatal(err)
	}
	r.Options.Output.OCI = filepath.Join(link, "image")
	if err := validate(r); err == nil {
		t.Fatal("accepted output resolving inside store")
	}
}
func TestFrontendValidationAndNoFabricatedCache(t *testing.T) {
	r := request(t)
	r.Kind = "build"
	r.Program = nil
	r.Build = &api.Build{Files: []string{r.Options.ContextDir}}
	if err := validate(r); err == nil {
		t.Fatal("directory accepted as definition")
	}
	file := filepath.Join(r.Options.ContextDir, "build.coopr")
	if err := os.WriteFile(file, []byte("from \"scratch\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r.Build.Files = []string{file}
	if err := validate(r); err != nil {
		t.Fatal(err)
	}
	r.Options.Output.Exports = []api.FileExport{{State: "named", Source: "/report", Destination: filepath.Join(filepath.Dir(r.Options.ContextDir), "report")}}
	if err := validate(r); err == nil {
		t.Fatal("native definition accepted typed-program state selector")
	}
	r.Options.Output.Exports = nil
	r.Kind = "component"
	r.Options.Output.Filesystem = filepath.Join(filepath.Dir(r.Options.ContextDir), "rootfs")
	if err := validate(r); err == nil {
		t.Fatal("component silently accepted rootfs output")
	}
}

func TestComponentStoreCannotOverlapResources(t *testing.T) {
	for _, kind := range []string{"build", "component", "publish"} {
		for _, which := range []string{"context", "graph", "run", "cache", "symlink"} {
			t.Run(kind+"/"+which, func(t *testing.T) {
				r := request(t)
				r.Kind = kind
				r.Program = nil
				root := filepath.Dir(r.Options.ContextDir)
				file := filepath.Join(r.Options.ContextDir, "build.coopr")
				if err := os.WriteFile(file, []byte("from scratch {}"), 0600); err != nil {
					t.Fatal(err)
				}
				r.Options.Cache.LocalDir = filepath.Join(root, "cache")
				selected := map[string]string{"context": r.Options.ContextDir, "graph": r.Options.Store.GraphRoot, "run": r.Options.Store.RunRoot, "cache": r.Options.Cache.LocalDir}[which]
				if which == "symlink" {
					selected = filepath.Join(root, "alias")
					if err := os.Symlink(r.Options.ContextDir, selected); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "publish" {
					r.Publish = &api.Publish{Artifact: "component", Source: "sha256:abc", Destinations: []string{"registry.test/team/item:v1"}, ComponentStoreDir: selected}
					r.Options.Output = api.Output{}
				} else {
					r.Build = &api.Build{Files: []string{file}, ComponentStoreDir: selected}
				}
				if err := validate(r); err == nil {
					t.Fatal("accepted overlapping component store")
				}
			})
		}
	}
}

func TestSymlinkComponentStoreCannotHideOutputOverlap(t *testing.T) {
	for _, kind := range []string{"build", "component", "publish"} {
		t.Run(kind, func(t *testing.T) {
			r := request(t)
			r.Program = nil
			r.Kind = kind
			root := filepath.Dir(r.Options.ContextDir)
			store := filepath.Join(root, "components")
			if err := os.Mkdir(store, 0700); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(root, "alias")
			if err := os.Symlink(store, alias); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(store, "export")
			if kind == "publish" {
				r.Options.Output = api.Output{}
				r.Publish = &api.Publish{Artifact: "component", Source: "sha256:abc", ComponentStoreDir: alias, Destinations: []string{"oci-archive:" + destination}}
			} else {
				file := filepath.Join(r.Options.ContextDir, "build.coopr")
				if err := os.WriteFile(file, []byte("from scratch {}"), 0600); err != nil {
					t.Fatal(err)
				}
				r.Build = &api.Build{Files: []string{file}, ComponentStoreDir: alias}
				r.Options.Output.OCI = destination
			}
			if err := validate(r); err == nil {
				t.Fatal("accepted output inside canonical component store")
			}
		})
	}
}

func TestPublicationRejectsExistingLocalDestinations(t *testing.T) {
	for _, transport := range []string{"oci", "oci-archive", "dir", "docker-archive"} {
		for _, form := range []string{"file", "directory", "symlink"} {
			t.Run(transport+"/"+form, func(t *testing.T) {
				r := request(t)
				r.Kind, r.Program, r.Options.Output = "publish", nil, api.Output{}
				destination := filepath.Join(filepath.Dir(r.Options.ContextDir), "published")
				switch form {
				case "file":
					if err := os.WriteFile(destination, []byte("caller-owned"), 0600); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(destination, 0700); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink("missing-target", destination); err != nil {
						t.Fatal(err)
					}
				}
				r.Publish = &api.Publish{Artifact: "image", Source: "fixture-image", Destinations: []string{transport + ":" + destination}}
				if err := validate(r); err == nil {
					t.Fatal("API publication accepted an existing caller-owned destination")
				}
			})
		}
	}
}

func publicationFixture(t *testing.T, path, form string) os.FileInfo {
	t.Helper()
	var err error
	switch form {
	case "file":
		err = os.WriteFile(path, []byte("caller-owned"), 0600)
	case "directory":
		err = os.Mkdir(path, 0700)
		if err == nil {
			err = os.WriteFile(filepath.Join(path, "keep"), []byte("caller-owned"), 0600)
		}
	case "symlink":
		err = os.Symlink("missing-caller-target", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func assertPublicationPreserved(t *testing.T, path, form string, original os.FileInfo) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !os.SameFile(original, info) {
		t.Fatalf("publication replaced caller inode: info=%v err=%v", info, err)
	}
	if form == "symlink" {
		target, err := os.Readlink(path)
		if err != nil || target != "missing-caller-target" {
			t.Fatalf("caller symlink changed: %q %v", target, err)
		}
		return
	}
	if form == "directory" {
		path = filepath.Join(path, "keep")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "caller-owned" {
		t.Fatalf("caller data changed: %q %v", data, err)
	}
}

func assertNoPublicationStaging(t *testing.T, parent string) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".coopr-publish-") {
			t.Fatalf("publication staging survived: %s", entry.Name())
		}
	}
}

func TestPublicationPreservesExistingAndRacingDestinations(t *testing.T) {
	for _, timing := range []string{"before-transfer", "during-transfer"} {
		for _, form := range []string{"file", "directory", "symlink"} {
			t.Run(timing+"/"+form, func(t *testing.T) {
				parent := t.TempDir()
				destination := transfer.Destination{Transport: "oci-archive", Name: filepath.Join(parent, "archive.tar")}
				var original os.FileInfo
				if timing == "before-transfer" {
					original = publicationFixture(t, destination.Name, form)
				}
				calls := 0
				reference, err := publishDestination(context.Background(), destination, func(_ context.Context, staging transfer.Destination) (string, error) {
					calls++
					if timing == "before-transfer" {
						t.Fatal("existing destination reached transfer")
					}
					// This creator runs after freshness validation and transfer admission,
					// deterministically covering the check-versus-promotion race.
					original = publicationFixture(t, destination.Name, form)
					if err := os.WriteFile(staging.Name, []byte("built artifact"), 0600); err != nil {
						t.Fatal(err)
					}
					return staging.Name, nil
				})
				if err == nil || reference != "" || calls != map[string]int{"before-transfer": 0, "during-transfer": 1}[timing] {
					t.Fatalf("existing destination accepted: reference=%q err=%v calls=%d", reference, err, calls)
				}
				assertPublicationPreserved(t, destination.Name, form, original)
				assertNoPublicationStaging(t, parent)
			})
		}
	}
}

func TestPublicationPromotesCompletedLocalArtifacts(t *testing.T) {
	for _, transport := range []string{"oci", "oci-archive", "dir", "docker-archive"} {
		t.Run(transport, func(t *testing.T) {
			parent := t.TempDir()
			destination := transfer.Destination{Transport: transport, Name: filepath.Join(parent, "white space ☃"), ArchiveReference: "app:release"}
			directory := transport == "oci" || transport == "dir"
			reference, err := publishDestination(context.Background(), destination, func(_ context.Context, staging transfer.Destination) (string, error) {
				if staging.Name == destination.Name || staging.Transport != destination.Transport || staging.ArchiveReference != destination.ArchiveReference {
					t.Fatalf("transfer destination changed beyond owned staging: %#v", staging)
				}
				info, err := os.Stat(filepath.Dir(staging.Name))
				if err != nil || info.Mode().Perm() != 0700 {
					t.Fatalf("staging is not private: %v %v", info, err)
				}
				path := staging.Name
				if directory {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(path, "index.json")
				}
				if err := os.WriteFile(path, []byte("complete artifact"), 0600); err != nil {
					t.Fatal(err)
				}
				return staging.String() + "@sha256:unchanged", nil
			})
			if err != nil || reference != destination.String()+"@sha256:unchanged" {
				t.Fatalf("promotion reference=%q err=%v", reference, err)
			}
			path := destination.Name
			if directory {
				path = filepath.Join(path, "index.json")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "complete artifact" {
				t.Fatalf("promoted bytes=%q err=%v", data, err)
			}
			assertNoPublicationStaging(t, parent)
		})
	}
}

func TestPublicationDoesNotPromoteFailedCanceledOrMissingArtifacts(t *testing.T) {
	for _, mode := range []string{"transfer-failed", "canceled", "missing-artifact"} {
		t.Run(mode, func(t *testing.T) {
			parent := t.TempDir()
			keep := filepath.Join(parent, "caller-file")
			original := publicationFixture(t, keep, "file")
			destination := transfer.Destination{Transport: "oci-archive", Name: filepath.Join(parent, "output.tar")}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("transfer failed after partial write")
			reference, err := publishDestination(ctx, destination, func(_ context.Context, staging transfer.Destination) (string, error) {
				if mode != "missing-artifact" {
					if err := os.WriteFile(staging.Name, []byte("partial artifact"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "transfer-failed" {
					return staging.Name, failure
				}
				if mode == "canceled" {
					cancel()
				}
				return staging.Name, nil
			})
			if err == nil || reference != "" {
				t.Fatalf("incomplete artifact promoted: reference=%q err=%v", reference, err)
			}
			if mode == "transfer-failed" && !errors.Is(err, failure) || mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost original failure: %v", err)
			}
			if _, err := os.Lstat(destination.Name); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed publication left final path: %v", err)
			}
			assertPublicationPreserved(t, keep, "file", original)
			assertNoPublicationStaging(t, parent)
		})
	}
}

func TestPublicationLeavesNonFilesystemTransfersUnchanged(t *testing.T) {
	for _, transport := range []string{"registry", "docker", "containers-storage", "local"} {
		destination := transfer.Destination{Transport: transport, Name: "authored-name"}
		const reference = "unchanged-reference"
		got, err := publishDestination(context.Background(), destination, func(_ context.Context, actual transfer.Destination) (string, error) {
			if actual != destination {
				t.Errorf("non-filesystem destination changed: %#v", actual)
			}
			return reference, nil
		})
		if err != nil || got != reference {
			t.Fatalf("non-filesystem result changed: %q %v", got, err)
		}
	}
}

func TestNamedCopyContextsReachPublicProgram(t *testing.T) {
	r := request(t)
	input := filepath.Join(filepath.Dir(r.Options.ContextDir), "assets")
	if err := os.Mkdir(input, 0700); err != nil {
		t.Fatal(err)
	}
	r.Options.Contexts = map[string]string{"stoke-assets": input}
	r.Program.States[0].Operations = []api.Operation{{Kind: "copy", Sources: []string{"scripts/$literal.sh"}, Destination: "/check.sh", From: api.Source{Context: "stoke-assets"}}}
	if err := validate(r); err != nil {
		t.Fatal(err)
	}
	p, err := program(*r.Program)
	if err != nil {
		t.Fatal(err)
	}
	copy := p.States[0].Operations[0].(coopr.Copy)
	if copy.From.Context != "stoke-assets" || copy.Sources[0] != "scripts/$literal.sh" {
		t.Fatalf("named source/literal changed: %#v", copy)
	}
	r.Program.States[0].Operations[0].From.Context = "unknown"
	if err := validate(r); err == nil {
		t.Fatal("unknown named COPY context accepted")
	}
}

func TestNamedContextsValidateBeforeNativeMutation(t *testing.T) {
	for _, kind := range []string{"execute", "build", "component"} {
		for _, scenario := range []string{"relative", "missing", "file", "reserved", "upper", "store", "cache", "component-store", "output", "symlink-output"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				r := request(t)
				r.Kind = kind
				root := filepath.Dir(r.Options.ContextDir)
				input := filepath.Join(root, "assets")
				if err := os.Mkdir(input, 0700); err != nil {
					t.Fatal(err)
				}
				r.Options.Contexts = map[string]string{"assets": input}
				if kind != "execute" {
					file := filepath.Join(r.Options.ContextDir, "build.coopr")
					if err := os.WriteFile(file, []byte("from scratch {}"), 0600); err != nil {
						t.Fatal(err)
					}
					r.Program = nil
					r.Build = &api.Build{Files: []string{file}, ComponentStoreDir: filepath.Join(root, "components")}
				}
				switch scenario {
				case "relative":
					r.Options.Contexts["assets"] = "relative"
				case "missing":
					r.Options.Contexts["assets"] = filepath.Join(root, "missing")
				case "file":
					file := filepath.Join(root, "file")
					if err := os.WriteFile(file, []byte("not directory"), 0600); err != nil {
						t.Fatal(err)
					}
					r.Options.Contexts["assets"] = file
				case "reserved":
					r.Options.Contexts = map[string]string{"build": input}
				case "upper":
					r.Options.Contexts = map[string]string{"ASSETS": input}
				case "store":
					r.Options.Store.GraphRoot = filepath.Join(input, "graph")
				case "cache":
					r.Options.Cache.LocalDir = filepath.Join(input, "cache")
				case "component-store":
					if r.Build == nil {
						r.Options.Contexts["assets"] = r.Options.Store.RunRoot
						break
					}
					r.Build.ComponentStoreDir = filepath.Join(input, "components")
				case "output":
					r.Options.Output.OCI = filepath.Join(input, "output")
				case "symlink-output":
					alias := filepath.Join(root, "alias")
					if err := os.Symlink(input, alias); err != nil {
						t.Fatal(err)
					}
					r.Options.Output.OCI = filepath.Join(alias, "output")
				}
				_, err := Execute(context.Background(), r, nil, nil)
				var failure *api.Error
				if !errors.As(err, &failure) || failure.Code != "invalid_request" {
					t.Fatalf("validation escaped into execution: %v", err)
				}
				if _, err := os.Lstat(r.Options.Store.RunRoot); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("validation mutated native store: %v", err)
				}
			})
		}
	}
}

func TestNamedContextMaySharePrimaryInputButNotPublicationOutput(t *testing.T) {
	r := request(t)
	r.Options.Contexts = map[string]string{"assets": r.Options.ContextDir, "same-input": r.Options.ContextDir}
	if err := validate(r); err != nil {
		t.Fatalf("input roots cannot intentionally overlap: %v", err)
	}
	r.Kind = "publish"
	r.Program = nil
	r.Options.Output = api.Output{}
	r.Publish = &api.Publish{Artifact: "image", Source: "image", Destinations: []string{"oci:" + filepath.Join(r.Options.ContextDir, "delivery")}}
	if err := validate(r); err == nil {
		t.Fatal("publication inside named input accepted")
	}
}

func TestPublicationRetainsCleanupUnconfirmedStaging(t *testing.T) {
	parent := t.TempDir()
	final := filepath.Join(parent, "rootfs")
	var staged string
	_, err := publishDestination(t.Context(), transfer.Destination{Transport: "dir", Name: final}, func(_ context.Context, target transfer.Destination) (string, error) {
		staged = target.Name
		if err := os.Mkdir(staged, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(staged, "input"), []byte("retained"), 0600); err != nil {
			t.Fatal(err)
		}
		return "", errors.Join(context.Canceled, buildah.ErrWorkerWaitIncomplete)
	})
	if !errors.Is(err, buildah.ErrWorkerWaitIncomplete) || !strings.Contains(err.Error(), filepath.Dir(staged)) {
		t.Fatalf("cleanup uncertainty or retained path lost: %v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(staged, "input"))
	if readErr != nil || string(data) != "retained" {
		t.Fatalf("uncertain worker input removed: %q %v", data, readErr)
	}
	if _, err := os.Lstat(final); !os.IsNotExist(err) {
		t.Fatalf("uncertain output promoted: %v", err)
	}
}
