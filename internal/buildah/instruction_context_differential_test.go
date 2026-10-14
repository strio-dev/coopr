package buildah

import (
	"archive/tar"
	"context"
	"coopr/internal/buildcontext"
	"coopr/internal/definition"
	"coopr/internal/imageconfig"
	"encoding/json"
	"errors"
	"fmt"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/storage/pkg/archive"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"coopr/internal/onbuildparse"
	"coopr/internal/planner"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/openshift/imagebuilder"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/imagebuildah"
)

// This compares actual pinned Builder.Run mount parsing and container execution,
// not the imagebuilder executor which only retains mount strings. Context and
// credentials live exclusively in the fixture's temporary directories.
func TestInstructionMountBackendDifferential(t *testing.T) {
	f := newLocalComponentFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	f.ctx = ctx
	base := newLiveBusyBoxStorage(t, ctx, f.root, f.options.Store)
	f.write(t, "data/payload", "mounted\n")
	f.write(t, "other/payload", "other\n")
	secret := filepath.Join(f.contextDir, "token")
	f.write(t, "token", "secret\n")
	lease, err := acquireStore(f.options.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	}()
	network, err := newNetworkInterface(lease.store)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string][]string{
		"bind":   {"source", "src", "target", "dst", "destination", "from", "ro", "rw", "readwrite", "readonly", "bind-propagation", "relabel", "z", "Z", "U", "nosuid", "nodev", "noexec", "bind-nonrecursive"},
		"cache":  {"id", "sharing", "mode", "uid", "gid", "source", "from", "target", "ro", "rw", "nosuid", "nodev", "noexec", "U"},
		"tmpfs":  {"target", "tmpfs-mode", "tmpfs-size", "ro", "rw"},
		"secret": {"id", "target", "required", "mode", "uid", "gid", "env"},
		"ssh":    {"id", "target", "required", "mode", "uid", "gid"},
	}
	for _, kind := range []string{"bind", "cache", "tmpfs", "secret", "ssh"} {
		for _, field := range fields[kind] {
			for _, variant := range []string{"valid", "empty", "invalid", "duplicate", "dynamic"} {
				t.Run(kind+"/"+field+"/"+variant, func(t *testing.T) {
					value := instructionMountFieldValue(kind, field)
					replacement := field + "=" + value
					env := []string{"VALUE=" + value}
					switch variant {
					case "empty":
						replacement = field + "="
					case "invalid":
						replacement = field + "=invalid-value"
					case "duplicate":
						replacement += "," + field + "=" + value
					case "dynamic":
						replacement = field + "=$VALUE"
					}
					mount := "type=" + kind + ",target=/mounted"
					if kind == "bind" {
						mount += ",source=data"
					}
					if kind == "cache" {
						mount += ",id=instruction-differential"
					}
					if kind == "secret" {
						mount += ",id=token"
					}
					// Replace the default rather than accidentally testing a duplicate in all cells.
					parts := strings.Split(mount, ",")
					var retained []string
					for _, part := range parts {
						key, _, _ := strings.Cut(part, "=")
						if key != field {
							retained = append(retained, part)
						}
					}
					mount = strings.Join(retained, ",") + "," + replacement
					expanded, nativeErr := imagebuilder.ProcessWord(mount, env)
					var nativeProof, cooprProof string
					for _, native := range []bool{true, false} {
						builder, err := upstream.NewBuilder(ctx, lease.store, upstream.BuilderOptions{FromImage: base.reference, PullPolicy: define.PullNever, Isolation: define.IsolationOCIRootless, Format: define.OCIv1ImageManifest, NetworkInterface: network, SystemContext: f.options.SystemContext})
						if err != nil {
							t.Fatal(err)
						}
						func() {
							defer func() {
								if err := builder.Delete(); err != nil {
									t.Error(err)
								}
							}()
							if native {
								if nativeErr == nil {
									secrets, err := parseOperationSecrets([]string{"id=token,src=" + secret})
									if err != nil {
										t.Fatal(err)
									}
									nativeErr = builder.Run([]string{"/bin/sh", "-c", "printf executed >/instruction-proof"}, upstream.RunOptions{Isolation: define.IsolationOCIRootless, Runtime: "crun", ConfigureNetwork: define.NetworkDisabled, ContextDir: f.contextDir, RunMounts: []string{expanded}, Secrets: secrets, Stdout: io.Discard, Stderr: io.Discard})
								}
								if nativeErr == nil {
									nativeProof = instructionBuilderFile(t, builder, "instruction-proof")
								}
								return
							}
							parsed, cooprErr := onbuildparse.ParseExpandedEnvironment("RUN --mount="+mount+" printf executed >/instruction-proof", env)
							if cooprErr == nil {
								operations, err := lowerOperationsWithCacheMountIDs([]planner.Operation{{Instruction: parsed[0]}}, func(planner.Operation, int) (string, error) { return "instruction-differential", nil })
								cooprErr = err
								if cooprErr == nil {
									run := operations[0].(Run)
									run.Network = "none"
									run.SecretSpecs = []string{"id=token,src=" + secret}
									run.Stdout = io.Discard
									run.Stderr = io.Discard
									// Fixed fixture cache identity; production normally resolves this from immutable inputs.
									for i := range run.Mounts {
										if run.Mounts[i].Type == "cache" && run.Mounts[i].Properties["id"] == "" {
											run.Mounts[i].Properties["id"] = "instruction-differential"
										}
									}
									cooprErr = run.apply(nativeBuilder{Builder: builder, isolation: define.IsolationOCIRootless, runtime: "crun"}, f.contextDir)
								}
							}
							if cooprErr == nil {
								cooprProof = instructionBuilderFile(t, builder, "instruction-proof")
							}
							t.Logf("INSTRUCTION_CASE id=run.open-mount-%s-%s variant=%s mount=%q native_error=%q coopr_error=%q native_bytes=%q coopr_bytes=%q", kind, field, variant, mount, fmt.Sprint(nativeErr), fmt.Sprint(cooprErr), nativeProof, cooprProof)
							if (nativeErr == nil) != (cooprErr == nil) || nativeProof != cooprProof {
								t.Errorf("native=%v/%q Coopr=%v/%q", nativeErr, nativeProof, cooprErr, cooprProof)
							}
						}()
					}
				})
			}
		}
	}
}

func instructionMountFieldValue(kind, field string) string {
	switch field {
	case "source", "src":
		return "data"
	case "target", "dst", "destination":
		return "/mounted"
	case "from":
		return "missing-stage"
	case "id":
		if kind == "secret" {
			return "token"
		}
		return "instruction-differential"
	case "sharing":
		return "shared"
	case "mode", "tmpfs-mode":
		return "0700"
	case "uid", "gid":
		return "0"
	case "tmpfs-size":
		return "1m"
	case "env":
		return "TOKEN"
	case "bind-propagation":
		return "rprivate"
	case "relabel":
		return "shared"
	case "required":
		return "false"
	default:
		return "true"
	}
}

func instructionBuilderFile(t *testing.T, builder *upstream.Builder, name string) string {
	t.Helper()
	mount, err := builder.Mount(builder.MountLabel)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(mount, name))
	unmountErr := builder.Unmount()
	if err != nil || unmountErr != nil {
		t.Fatalf("read proof=%v unmount=%v", err, unmountErr)
	}
	return string(data)
}

// Native builds below use the public imagebuildah graph/context executor. This
// catches differences hidden by a frontend-only recorder or isolated lowering.
func TestInstructionGraphContextDifferential(t *testing.T) {
	cases := []struct {
		name, dockerfile, kdl  string
		args                   map[string]string
		namedContext, external bool
	}{
		{name: "global-stage-undeclared", dockerfile: "ARG A=global\nFROM scratch\nENV RESULT=$A\nCOPY payload /proof\n", kdl: "arg \"A\" \"global\"\nfrom \"scratch\" { env RESULT=\"$A\"; copy \"payload\" \"/proof\"; }"},
		{name: "global-stage-redeclared", dockerfile: "ARG A=global\nFROM scratch\nARG A\nENV RESULT=$A\nCOPY payload /proof\n", kdl: "arg \"A\" \"global\"\nfrom \"scratch\" { arg \"A\"; env RESULT=\"$A\"; copy \"payload\" \"/proof\"; }"},
		{name: "from-global-expansion", dockerfile: "ARG BASE=scratch\nFROM $BASE\nCOPY payload /proof\n", kdl: "arg \"BASE\" \"scratch\"\nfrom \"$BASE\" {copy \"payload\" \"/proof\";}"},
		{name: "cross-stage-arg", dockerfile: "FROM scratch AS first\nARG A=first\nENV KEEP=$A\nCOPY payload /proof\nFROM first\nARG A\nENV RESULT=$A\n", kdl: "from \"scratch\" as=\"first\" {arg \"A\" \"first\";env KEEP=\"$A\";copy \"payload\" \"/proof\";}\nfrom \"first\" {arg \"A\";env RESULT=\"$A\";}"},
		{name: "target-platform-override", dockerfile: "FROM scratch\nARG TARGETARCH\nENV RESULT=$TARGETARCH\nCOPY payload /proof\n", kdl: "from \"scratch\" {arg \"TARGETARCH\";env RESULT=\"$TARGETARCH\";copy \"payload\" \"/proof\";}", args: map[string]string{"TARGETARCH": "custom"}},
		{name: "copy-from-stage", dockerfile: "FROM scratch AS source\nCOPY payload /payload\nFROM scratch\nCOPY --from=source /payload /proof\n", kdl: "from \"scratch\" as=\"source\" {copy \"payload\" \"/payload\";}\nfrom \"scratch\" {copy \"/payload\" \"/proof\" from=\"source\";}"},
		{name: "copy-from-named-context", namedContext: true, dockerfile: "FROM scratch\nCOPY --from=named payload /proof\n", kdl: `from "scratch" {copy "payload" "/proof" from="named";}`},
		{name: "copy-from-external-image", external: true, dockerfile: "FROM scratch\nCOPY --from=fixture.local/external:latest /payload /proof\n", kdl: `from "scratch" {copy "/payload" "/proof" from="fixture.local/external:latest";}`},
		{name: "copy-from-dynamic-external-image", external: true, dockerfile: "FROM scratch\nARG BASE=fixture.local/external:latest\nCOPY --from=$BASE /payload /proof\n", kdl: `from "scratch" {arg "BASE" "fixture.local/external:latest";copy "/payload" "/proof" from="$BASE";}`},
		{name: "run-bind-from-stage", dockerfile: "FROM scratch AS source\nCOPY payload /payload\nFROM @BASE@\nRUN --network=none --mount=type=bind,from=source,source=/payload,target=/mounted cat /mounted >/proof\n", kdl: `from "scratch" as="source" {copy "payload" "/payload";}
from "@BASE@" {run "cat /mounted >/proof" network="none" {mount "bind" from="source" source="/payload" target="/mounted";};}`},
		{name: "run-context-bind", dockerfile: "FROM @BASE@\nRUN --network=none --mount=type=bind,target=/mounted cat /mounted/payload >/proof\n", kdl: `from "@BASE@" {run "cat /mounted/payload >/proof" network="none" {mount "bind" target="/mounted";};}`},
		{name: "proxy-builtins", args: map[string]string{"HTTP_PROXY": "context bytes"}, dockerfile: "FROM @BASE@\nRUN --network=none printf '%s\\n' \"$HTTP_PROXY\" >/proof\n", kdl: `from "@BASE@" {run "printf '%s\\n' \"$HTTP_PROXY\" >/proof" network="none";}`},
		{name: "runtime-environment-user-workdir-shell", dockerfile: "FROM @BASE@\nENV VALUE=bytes\nUSER 0:0\nWORKDIR /work\nSHELL [\"/bin/sh\",\"-ec\"]\nRUN --network=none test \"$(/bin/busybox id -u)\" = 0 && test \"$PWD\" = /work && printf 'context %s\\n' \"$VALUE\" >/proof\n", kdl: `from "@BASE@" {env VALUE="bytes";user "0:0";workdir "/work";shell "/bin/sh" "-ec";run "test \"$(/bin/busybox id -u)\" = 0 && test \"$PWD\" = /work && printf 'context %s\\n' \"$VALUE\" >/proof" network="none";}`},
		{name: "run-inline-script", dockerfile: "FROM @BASE@\nRUN --network=none <<EOF\nprintf 'context bytes\\n' >/proof\nEOF\n", kdl: `from "@BASE@" {run "printf 'context bytes\\n' >/proof" network="none";}`},
		{name: "onbuild-context", dockerfile: "FROM scratch AS parent\nONBUILD COPY payload /proof\nFROM parent\n", kdl: `from "scratch" as="parent" {onbuild {copy "payload" "/proof";};}
from "parent" {}`},
		{name: "run-cache-from-stage", dockerfile: "FROM scratch AS source\nCOPY payload /seed/payload\nFROM @BASE@\nRUN --network=none --mount=type=cache,from=source,source=/seed,target=/mounted,id=seed cat /mounted/payload >/proof\n", kdl: "from \"scratch\" as=\"source\" {copy \"payload\" \"/seed/payload\";}\nfrom \"@BASE@\" {run \"cat /mounted/payload >/proof\" network=\"none\" {mount \"cache\" from=\"source\" source=\"/seed\" target=\"/mounted\" id=\"seed\";};}"},
		{name: "run-bind-from-named-context", dockerfile: "FROM @BASE@\nRUN --network=none --mount=type=bind,from=named,target=/mounted,relabel=shared cat /mounted/payload >/proof\n", kdl: "from \"@BASE@\" {run \"cat /mounted/payload >/proof\" network=\"none\" {mount \"bind\" from=\"named\" target=\"/mounted\" relabel=\"shared\";};}", namedContext: true},
		{name: "run-bind-from-external-image", dockerfile: "FROM @BASE@\nRUN --network=none --mount=type=bind,from=fixture.local/external:latest,source=/payload,target=/mounted cat /mounted >/proof\n", kdl: "from \"@BASE@\" {run \"cat /mounted >/proof\" network=\"none\" {mount \"bind\" from=\"fixture.local/external:latest\" source=\"/payload\" target=\"/mounted\";};}", external: true},
		{name: "metadata-roundtrip", dockerfile: "FROM scratch\nLABEL example=value\nMAINTAINER Person\nEXPOSE 80/udp\nVOLUME /data\nCMD [\"echo\",\"command\"]\nENTRYPOINT [\"entry\"]\nSHELL [\"/bin/sh\",\"-ec\"]\nSTOPSIGNAL SIGTERM\nHEALTHCHECK --interval=1s CMD [\"true\"]\nCOPY payload /proof\n", kdl: "from \"scratch\" {label example=\"value\";maintainer \"Person\";expose \"80/udp\";volume \"/data\";cmd {exec \"echo\" \"command\";};entrypoint {exec \"entry\";};shell \"/bin/sh\" \"-ec\";stopsignal \"SIGTERM\";healthcheck interval=\"1s\" {exec \"true\";};copy \"payload\" \"/proof\";}"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.write(t, "payload", "context bytes\n")
			if strings.Contains(test.dockerfile, "@BASE@") {
				base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
				test.dockerfile = strings.ReplaceAll(test.dockerfile, "@BASE@", base.reference)
				test.kdl = strings.ReplaceAll(test.kdl, "@BASE@", base.reference)
			}
			var contexts []buildcontext.Spec
			var nativeContexts map[string]*define.AdditionalBuildContext
			if test.namedContext {
				contexts = []buildcontext.Spec{{Name: "named", Kind: buildcontext.Local, Path: f.contextDir}}
				nativeContexts = map[string]*define.AdditionalBuildContext{"named": {Value: f.contextDir}}
			}
			if test.external {
				result, _ := f.build(t, "external", `from "scratch" {copy "payload" "/payload";}`)
				lease, err := acquireStore(f.options.Store)
				if err != nil {
					t.Fatal(err)
				}
				err = lease.store.AddNames(result.ImageID, []string{"fixture.local/external:latest"})
				closeErr := lease.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("name external=%v close=%v", err, closeErr)
				}
			}

			document, err := definition.Parse(strings.NewReader(test.kdl))
			if err != nil {
				t.Fatal(err)
			}

			plan, err := planner.Create(document, planner.Options{Mode: planner.Build, Platform: runtime.GOOS + "/" + runtime.GOARCH, Arguments: test.args, BuildContexts: contexts})
			if err != nil {
				t.Fatal(err)
			}
			options := f.options
			options.BuildContexts = contexts
			options.ProxyArgs = planner.PredefinedProxyArguments(test.args)
			layout := filepath.Join(f.root, "coopr")
			options.Output = Output{Path: layout}
			if _, err := BuildPlan(f.ctx, plan, options); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(f.contextDir, "Containerfile")
			f.write(t, "Containerfile", test.dockerfile)
			lease, err := acquireStore(f.options.Store)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := lease.Close(); err != nil {
					t.Error(err)
				}
			}()
			nativeLayout := filepath.Join(f.root, "native")
			_, _, err = imagebuildah.BuildDockerfiles(f.ctx, lease.store, define.BuildOptions{ContextDirectory: f.contextDir, Output: "oci:" + nativeLayout, OutputFormat: define.Dockerv2ImageManifest, Layers: true, Args: test.args, AdditionalBuildContexts: nativeContexts, PullPolicy: define.PullNever, Isolation: define.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &define.CommonBuildOptions{}, Compression: define.Uncompressed, Out: io.Discard, Err: os.Stderr, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, file)
			if err != nil {
				t.Fatal(err)
			}
			gotManifest, gotConfig := readPlanImage(t, layout)
			wantManifest, wantConfig := readPlanImage(t, nativeLayout)
			if !reflect.DeepEqual(gotConfig.Config.Env, wantConfig.Config.Env) {
				t.Fatalf("ENV Coopr=%v native=%v", gotConfig.Config.Env, wantConfig.Config.Env)
			}
			if test.name == "metadata-roundtrip" {
				// OCI export converts away native Docker-only extensions. Read the
				// public native builder for those fields before comparing logical metadata.
				type metadata struct {
					Labels                 map[string]string
					Ports                  map[string]struct{}
					Volumes                map[string]struct{}
					Cmd, Entrypoint, Shell []string
					StopSignal             string
					Healthcheck            *imageconfig.Healthcheck
				}
				project := func(layout string, manifest v1.Manifest) metadata {
					data, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", manifest.Config.Digest.Encoded()))
					if err != nil {
						t.Fatal(err)
					}
					var document struct {
						Config struct {
							Labels                 map[string]string
							ExposedPorts           map[string]struct{}
							Volumes                map[string]struct{}
							Cmd, Entrypoint, Shell []string
							StopSignal             string
							Healthcheck            *imageconfig.Healthcheck
						}
					}
					if err := json.Unmarshal(data, &document); err != nil {
						t.Fatal(err)
					}
					fields := document.Config
					return metadata{fields.Labels, fields.ExposedPorts, fields.Volumes, fields.Cmd, fields.Entrypoint, fields.Shell, fields.StopSignal, fields.Healthcheck}
				}
				expected := project(nativeLayout, wantManifest)
				const nativeReference = "fixture.local/native-metadata:latest"
				_, _, err = imagebuildah.BuildDockerfiles(f.ctx, lease.store, define.BuildOptions{ContextDirectory: f.contextDir, Output: nativeReference, OutputFormat: define.Dockerv2ImageManifest, Layers: true, PullPolicy: define.PullNever, Isolation: define.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &define.CommonBuildOptions{}, Out: io.Discard, Err: io.Discard, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, file)
				if err != nil {
					t.Fatal(err)
				}
				nativeBuilder, err := upstream.NewBuilder(f.ctx, lease.store, upstream.BuilderOptions{FromImage: nativeReference, PullPolicy: define.PullNever, SystemContext: f.options.SystemContext})
				if err != nil {
					t.Fatal(err)
				}
				expected.Shell = nativeBuilder.Shell()
				if health := nativeBuilder.Healthcheck(); health != nil {
					data, err := json.Marshal(health)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(data, &expected.Healthcheck); err != nil {
						t.Fatal(err)
					}
				}
				if err := nativeBuilder.Delete(); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(project(layout, gotManifest), expected) {
					t.Fatalf("metadata Coopr=%+v native=%+v", project(layout, gotManifest), expected)
				}
			}
			// The proof can live in an inherited base layer: inspect all layers, latest wins.
			proof := func(layout string, manifest v1.Manifest) string {
				for i := len(manifest.Layers) - 1; i >= 0; i-- {
					layer := filepath.Join(layout, "blobs", "sha256", manifest.Layers[i].Digest.Encoded())
					if data, ok := instructionOptionalLayerFile(t, layer, "proof"); ok {
						return data
					}
				}
				t.Fatal("missing proof")
				return ""
			}
			if got, want := proof(layout, gotManifest), proof(nativeLayout, wantManifest); got != want || got != "context bytes\n" {
				t.Fatalf("bytes Coopr=%q native=%q", got, want)
			}
			t.Logf("INSTRUCTION_CONTEXT case=%s native_and_coopr_bytes=%q env=%q", test.name, "context bytes\n", gotConfig.Config.Env)
		})
	}
}

func instructionOptionalLayerFile(t *testing.T, layer, name string) (string, bool) {
	t.Helper()
	file, err := os.Open(layer)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close layer file: %v", err)
		}
	}()
	stream, err := archive.DecompressStream(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			t.Errorf("close layer stream: %v", err)
		}
	}()
	reader := tar.NewReader(stream)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return "", false
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimPrefix(header.Name, "./") == name {
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			return string(data), true
		}
	}
}

func TestInstructionNativeRejections(t *testing.T) {
	for _, raw := range []string{"UNKNOWN x", "ADD --from=external:latest payload /proof", "ADD --from=$BASE payload /proof", "RUN --mount=type=bogus,target=/x true"} {
		t.Run(raw, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.write(t, "payload", "context bytes\n")
			base := "scratch"
			if strings.HasPrefix(raw, "RUN") {
				base = newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store).reference
			}
			f.write(t, "Containerfile", "FROM "+base+"\n"+raw+"\n")
			lease, err := acquireStore(f.options.Store)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := lease.Close(); err != nil {
					t.Error(err)
				}
			}()
			_, _, nativeErr := imagebuildah.BuildDockerfiles(f.ctx, lease.store, define.BuildOptions{ContextDirectory: f.contextDir, PullPolicy: define.PullNever, Isolation: define.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &define.CommonBuildOptions{}, Out: io.Discard, Err: io.Discard, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, filepath.Join(f.contextDir, "Containerfile"))
			_, cooprErr := onbuildparse.ParseDeferred(raw)
			t.Logf("INSTRUCTION_REJECTION raw=%q native_error=%q coopr_error=%q", raw, fmt.Sprint(nativeErr), fmt.Sprint(cooprErr))
			if nativeErr == nil || cooprErr == nil {
				t.Fatalf("native rejection=%v Coopr rejection=%v", nativeErr, cooprErr)
			}
		})
	}
}

func TestInstructionInjectedOnBuildNativeDifferential(t *testing.T) {
	for _, raw := range []string{"FROM scratch", "ONBUILD ENV LATER=value", "MAINTAINER Person", "UNKNOWN x", "RUN --network=none <<EOF\nprintf 'inline bytes\\n' >/inline-proof\nEOF\n"} {
		t.Run(raw, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.write(t, "payload", "context bytes\n")
			lease, err := acquireStore(f.options.Store)
			if err != nil {
				t.Fatal(err)
			}
			base := "scratch"
			if strings.HasPrefix(raw, "RUN") {
				if err := lease.Close(); err != nil {
					t.Fatal(err)
				}
				base = newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store).reference
				lease, err = acquireStore(f.options.Store)
				if err != nil {
					t.Fatal(err)
				}
			}
			builder, err := upstream.NewBuilder(f.ctx, lease.store, upstream.BuilderOptions{FromImage: base, PullPolicy: define.PullNever, Format: define.Dockerv2ImageManifest, SystemContext: f.options.SystemContext})
			if err != nil {
				t.Fatal(err)
			}
			builder.SetOnBuild(raw)
			imageID, _, _, err := builder.Commit(f.ctx, nil, upstream.CommitOptions{PreferredManifestType: define.Dockerv2ImageManifest, SystemContext: f.options.SystemContext})
			if err != nil {
				t.Fatal(err)
			}
			if err := builder.Delete(); err != nil {
				t.Fatal(err)
			}
			const reference = "fixture.local/injected-onbuild:latest"
			if err := lease.store.AddNames(imageID, []string{reference}); err != nil {
				t.Fatal(err)
			}
			f.write(t, "Containerfile", "FROM "+reference+"\nCOPY payload /proof\n")
			nativeLayout := filepath.Join(f.root, "native")
			_, _, nativeErr := imagebuildah.BuildDockerfiles(f.ctx, lease.store, define.BuildOptions{ContextDirectory: f.contextDir, Output: "oci:" + nativeLayout, OutputFormat: define.Dockerv2ImageManifest, PullPolicy: define.PullNever, Isolation: define.IsolationOCIRootless, Runtime: "crun", CommonBuildOpts: &define.CommonBuildOptions{}, SystemContext: f.options.SystemContext, Out: io.Discard, Err: io.Discard, RemoveIntermediateCtrs: true, ForceRmIntermediateCtrs: true}, filepath.Join(f.contextDir, "Containerfile"))
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			options := f.options
			options.Output = Output{Path: filepath.Join(f.root, "coopr")}
			_, cooprErr := BuildPlan(f.ctx, testPlan(t, fmt.Sprintf("from %q {copy \"payload\" \"/proof\";}", reference)), options)
			t.Logf("INSTRUCTION_INJECTED raw=%q native_error=%q coopr_error=%q", raw, fmt.Sprint(nativeErr), fmt.Sprint(cooprErr))
			if (nativeErr == nil) != (cooprErr == nil) {
				t.Fatalf("native=%v Coopr=%v", nativeErr, cooprErr)
			}
			if nativeErr == nil {
				if strings.HasPrefix(raw, "RUN") {
					for _, layout := range []string{nativeLayout, options.Output.Path} {
						manifest, _ := readPlanImage(t, layout)
						found := false
						for _, layer := range manifest.Layers {
							data, ok := instructionOptionalLayerFile(t, filepath.Join(layout, "blobs", "sha256", layer.Digest.Encoded()), "inline-proof")
							if ok {
								if data != "inline bytes\n" {
									t.Fatalf("inline bytes=%q", data)
								}
								found = true
							}
						}
						if !found {
							t.Fatalf("missing inline-proof in %s", layout)
						}
					}
				}
				if got := localComponentLastFile(t, nativeLayout, "proof"); got != "context bytes\n" {
					t.Fatalf("native proof=%q", got)
				}
				if got := localComponentLastFile(t, options.Output.Path, "proof"); got != "context bytes\n" {
					t.Fatalf("Coopr proof=%q", got)
				}
			}
		})
	}
}

func TestInstructionNativeIngestionPolicyAllowedTriggers(t *testing.T) {
	for _, raw := range []string{`ARG A=value`, `RUN true`, `RUN --device=vendor.example/gpu=all true`, `RUN --security=insecure true`, `COPY payload /proof`, `ADD --unpack=false payload /proof`, `ENV A=value`, `LABEL A=value`, `USER ""`, `WORKDIR ""`, `CMD ["true"]`, `ENTRYPOINT ["true"]`, `SHELL ["/bin/sh","-ec"]`, `EXPOSE 80`, `VOLUME /data`, `STOPSIGNAL SIGTERM`, `HEALTHCHECK NONE`} {
		t.Run(raw, func(t *testing.T) {
			node, err := imagebuilder.ParseDockerfile(strings.NewReader("FROM scratch\n"))
			if err != nil {
				t.Fatal(err)
			}
			nativeErr := imagebuilder.NewBuilder(nil).FromImage(&docker.Image{Config: &docker.Config{OnBuild: []string{raw}}}, node)
			data, err := json.Marshal(map[string]any{"config": map[string]any{"OnBuild": []string{raw}}})
			if err != nil {
				t.Fatal(err)
			}
			config, err := imageconfig.Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			_, cooprErr := stageBindFromImageConfig(config)
			if nativeErr != nil || cooprErr != nil {
				t.Fatalf("allowed trigger native=%v Coopr=%v", nativeErr, cooprErr)
			}
		})
	}
	// The restriction belongs to ingestion, while direct stage instructions remain legal.
	if _, err := onbuildparse.ParseDeferred("MAINTAINER Person"); err != nil {
		t.Fatalf("generic parser rejected direct stage MAINTAINER: %v", err)
	}
}
