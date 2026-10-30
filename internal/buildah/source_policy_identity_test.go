package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/oci"
	"go.podman.io/image/v5/copy"
	dockerreference "go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/image"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/signature/sigstore"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
	orasoci "oras.land/oras-go/v2/content/oci"
)

func TestFilesystemSourceKeepsNativeSignaturePolicyIdentity(t *testing.T) {
	for _, transport := range []string{"oci", "oci-archive", "dir", "docker-archive"} {
		for _, scopeKind := range []string{"exact", "parent", "transport", "global"} {
			if transport == "docker-archive" && (scopeKind == "exact" || scopeKind == "parent") {
				continue // Native docker-archive supports only the transport default scope.
			}
			for _, allow := range []bool{false, true} {
				name := transport + "/" + scopeKind
				if allow {
					name += "/allow"
				} else {
					name += "/reject"
				}
				t.Run(name, func(t *testing.T) {
					ctx := context.Background()
					root := t.TempDir()
					platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
					layout := filepath.Join(root, "input")
					source, err := orasoci.NewWithContext(ctx, layout)
					if err != nil {
						t.Fatal(err)
					}
					manifest, _ := sourceTestImage(t, ctx, source, platform, "policy-identity")
					if err := source.Tag(ctx, manifest, "latest"); err != nil {
						t.Fatal(err)
					}
					authored := "oci:input:latest"
					if transport != "oci" {
						authored = transport + ":exported"
						switch transport {
						case "oci-archive":
							authored += ":latest"
						case "docker-archive":
							authored += ":example:latest"
						}
						from, err := alltransports.ParseImageName("oci:" + layout + ":latest")
						if err != nil {
							t.Fatal(err)
						}
						toName, _, err := normalizeBaseSource(authored, root)
						if err != nil {
							t.Fatal(err)
						}
						to, err := alltransports.ParseImageName(toName)
						if err != nil {
							t.Fatal(err)
						}
						fixturePolicy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}})
						if err != nil {
							t.Fatal(err)
						}
						fixtureSystem := &types.SystemContext{BigFilesTemporaryDir: root}
						_, copyErr := copy.Image(ctx, fixturePolicy, to, from, &copy.Options{SourceCtx: fixtureSystem, DestinationCtx: fixtureSystem})
						closeErr := fixturePolicy.Destroy()
						if copyErr != nil || closeErr != nil {
							t.Fatalf("prepare source: %v; %v", copyErr, closeErr)
						}
					}
					normalized, _, err := normalizeBaseSource(authored, root)
					if err != nil {
						t.Fatal(err)
					}
					reference, err := alltransports.ParseImageName(normalized)
					if err != nil {
						t.Fatal(err)
					}
					defaultRequirement, scopedRequirement := "insecureAcceptAnything", "reject"
					if allow {
						defaultRequirement, scopedRequirement = scopedRequirement, defaultRequirement
					}
					scopes := map[string]any{}
					scope := reference.PolicyConfigurationIdentity()
					if scopeKind == "parent" {
						for _, parent := range reference.PolicyConfigurationNamespaces() {
							if parent != scope {
								scope = parent
								break
							}
						}
					} else if scopeKind == "transport" {
						scope = ""
					}
					if scopeKind != "global" {
						scopes[""] = []map[string]string{{"type": defaultRequirement}}
						if scopeKind != "transport" {
							for _, parent := range reference.PolicyConfigurationNamespaces() {
								if parent != reference.PolicyConfigurationIdentity() {
									scopes[parent] = []map[string]string{{"type": defaultRequirement}}
								}
							}
						}
						scopes[scope] = []map[string]string{{"type": scopedRequirement}}
					} else {
						defaultRequirement = scopedRequirement
					}
					data, err := json.Marshal(map[string]any{
						"default":    []map[string]string{{"type": defaultRequirement}},
						"transports": map[string]any{transport: scopes},
					})
					if err != nil {
						t.Fatal(err)
					}
					policyFile := filepath.Join(root, "policy.json")
					if err := os.WriteFile(policyFile, data, 0o600); err != nil {
						t.Fatal(err)
					}
					system := &types.SystemContext{SignaturePolicyPath: policyFile, BigFilesTemporaryDir: root}
					policy, err := signature.DefaultPolicy(system)
					if err != nil {
						t.Fatal(err)
					}
					nativePolicy, err := signature.NewPolicyContext(policy)
					if err != nil {
						t.Fatal(err)
					}
					nativeSource, err := reference.NewImageSource(ctx, system)
					if err != nil {
						t.Fatal(err)
					}
					nativeAllowed, nativeErr := nativePolicy.IsRunningImageAllowed(ctx, image.UnparsedInstance(nativeSource, nil))
					if err := nativeSource.Close(); err != nil {
						t.Fatal(err)
					}
					if err := nativePolicy.Destroy(); err != nil {
						t.Fatal(err)
					}
					if nativeAllowed != allow || (nativeErr == nil) != allow {
						t.Fatalf("native policy = %t, %v, want allow=%t", nativeAllowed, nativeErr, allow)
					}
					policySources := []string{"system"}
					// Scope selection varies by transport; effective-context merging
					// is shared. Exercise its other origins once per allow/reject case.
					if transport == "oci" && scopeKind == "exact" {
						policySources = append(policySources, "default-policy", "resolver", "system-overrides-resolver")
					}
					for _, policySource := range policySources {
						t.Run(policySource, func(t *testing.T) {
							storeRoot := t.TempDir()
							store, err := storage.GetStore(storage.StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(storeRoot, "graph"), RunRoot: filepath.Join(storeRoot, "run")})
							if err != nil {
								t.Fatal(err)
							}
							defer func() {
								if _, err := store.Shutdown(true); err != nil {
									t.Error(err)
								}
							}()
							var resolver *oci.Resolver
							supplied := system
							if policySource == "default-policy" {
								supplied = nil
								t.Setenv("CONTAINERS_POLICY_JSON", policyFile)
							} else if policySource != "system" {
								resolverPolicy := policyFile
								if policySource == "resolver" {
									supplied = nil
								} else {
									opposite := "insecureAcceptAnything"
									if allow {
										opposite = "reject"
									}
									resolverPolicy = filepath.Join(storeRoot, "conflicting-policy.json")
									if err := os.WriteFile(resolverPolicy, []byte(`{"default":[{"type":"`+opposite+`"}]}`), 0o600); err != nil {
										t.Fatal(err)
									}
								}
								resolver, err = oci.NewResolver(oci.Options{SignaturePolicyPath: resolverPolicy})
								if err != nil {
									t.Fatal(err)
								}
							}
							selected, err := resolveBaseSource(ctx, resolver, authored, platform, store, supplied, root, "")
							if (err == nil) != allow {
								t.Fatalf("Coopr policy error = %v, want native allow=%t", err, allow)
							}
							if allow && selected.Reference != normalized {
								t.Fatalf("selected source = %q, want %q", selected.Reference, normalized)
							}
							if system.SignaturePolicyPath != policyFile {
								t.Fatalf("caller policy changed to %q", system.SignaturePolicyPath)
							}
						})
					}
				})
			}
		}
	}
}

func TestSnapshotSourcePolicyKeepsAllRequirementsAndPrivateLifecycle(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"original", "snapshot"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	original, err := alltransports.ParseImageName("oci:" + filepath.Join(root, "original") + ":latest")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := "oci:" + filepath.Join(root, "snapshot") + ":latest"
	frozen, err := alltransports.ParseImageName(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	requirements := signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything(), signature.NewPRReject()}
	policy := &signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}, Transports: map[string]signature.PolicyTransportScopes{"oci": {original.PolicyConfigurationIdentity(): requirements}}}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	policyFile := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policyFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	path, cleanup, err := snapshotSourcePolicy(&types.SystemContext{SignaturePolicyPath: policyFile}, original, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(path) }()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("bound policy mode: %v, %v", info, err)
	}
	bound, err := signature.NewPolicyFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(bound.Transports["oci"][frozen.PolicyConfigurationIdentity()])
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(requirements)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) || len(bound.Transports) != 1 || len(bound.Transports["oci"]) != 1 {
		t.Fatalf("bound policy did not retain the complete exact-scope requirements: %s", got)
	}
	got, err = json.Marshal(bound.Default)
	if err != nil || string(got) != `[{"type":"reject"}]` {
		t.Fatalf("bound default = %s, %v", got, err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("bound policy remains after cleanup: %v", err)
	}
	unchanged, err := os.ReadFile(policyFile)
	if err != nil || !bytes.Equal(unchanged, data) {
		t.Fatalf("original policy changed: %v", err)
	}
}

func TestSnapshotSourcePolicyRejectsChangedDockerIdentity(t *testing.T) {
	root := t.TempDir()
	original, err := alltransports.ParseImageName("docker-archive:" + filepath.Join(root, "original.tar") + ":example:original")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := snapshotSourcePolicy(&types.SystemContext{}, original, "docker-archive:"+filepath.Join(root, "snapshot.tar")+":example:changed"); err == nil || !strings.Contains(err.Error(), "Docker identity") {
		t.Fatalf("changed Docker identity accepted: %v", err)
	}
}

func TestConfinedSourcePolicyReferenceUsesCanonicalContextRoot(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "image"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	reference, contextRoot, err := confinedSourcePolicyReference("oci:"+filepath.Join(alias, "image")+":latest", alias)
	if err != nil {
		t.Fatal(err)
	}
	if contextRoot != real || reference.PolicyConfigurationIdentity() != filepath.Join(real, "image") || !strings.HasSuffix(reference.StringWithinTransport(), ":latest") {
		t.Fatalf("confined policy reference = %s, root=%s", reference.StringWithinTransport(), contextRoot)
	}
}

func TestFilesystemSourcePreservesNativeSigstoreVerification(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	layout := filepath.Join(root, "input")
	source, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := sourceTestImage(t, ctx, source, platform, "signed-source")
	if err := source.Tag(ctx, manifest, "latest"); err != nil {
		t.Fatal(err)
	}
	keys, err := sigstore.GenerateKeyPair([]byte("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(root, "fixture.key")
	if err := os.WriteFile(keyFile, keys.PrivateKey, 0o600); err != nil {
		t.Fatal(err)
	}
	from, err := alltransports.ParseImageName("oci:" + layout + ":latest")
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "signed")
	to, err := alltransports.ParseImageName("dir:" + directory)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := dockerreference.ParseNormalizedNamed("example.test/signed:latest")
	if err != nil {
		t.Fatal(err)
	}
	fixturePolicy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}})
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := copy.Image(ctx, fixturePolicy, to, from, &copy.Options{SignIdentity: identity, SignBySigstorePrivateKeyFile: keyFile, SignSigstorePrivateKeyPassphrase: []byte("fixture")})
	closeErr := fixturePolicy.Destroy()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("sign fixture: %v; %v", copyErr, closeErr)
	}
	store, err := storage.GetStore(storage.StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(root, "graph"), RunRoot: filepath.Join(root, "run")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := store.Shutdown(true); err != nil {
			t.Error(err)
		}
	}()
	for _, trusted := range []bool{true, false} {
		publicKey := keys.PublicKey
		if !trusted {
			wrongKeys, err := sigstore.GenerateKeyPair([]byte("wrong"))
			if err != nil {
				t.Fatal(err)
			}
			publicKey = wrongKeys.PublicKey
		}
		match, err := signature.NewPRMExactReference(identity.String())
		if err != nil {
			t.Fatal(err)
		}
		requirement, err := signature.NewPRSigstoreSignedKeyData(publicKey, match)
		if err != nil {
			t.Fatal(err)
		}
		policy := &signature.Policy{Default: signature.PolicyRequirements{signature.NewPRReject()}, Transports: map[string]signature.PolicyTransportScopes{"dir": {to.PolicyConfigurationIdentity(): {requirement}}}}
		data, err := json.Marshal(policy)
		if err != nil {
			t.Fatal(err)
		}
		policyFile := filepath.Join(root, "policy.json")
		if err := os.WriteFile(policyFile, data, 0o600); err != nil {
			t.Fatal(err)
		}
		system := &types.SystemContext{SignaturePolicyPath: policyFile}
		nativePolicy, err := signature.NewPolicyContext(policy)
		if err != nil {
			t.Fatal(err)
		}
		nativeSource, err := to.NewImageSource(ctx, system)
		if err != nil {
			t.Fatal(err)
		}
		allowed, nativeErr := nativePolicy.IsRunningImageAllowed(ctx, image.UnparsedInstance(nativeSource, nil))
		if err := nativeSource.Close(); err != nil {
			t.Fatal(err)
		}
		if err := nativePolicy.Destroy(); err != nil {
			t.Fatal(err)
		}
		if allowed != trusted || (nativeErr == nil) != trusted {
			t.Fatalf("native signature verification = %t, %v, want trusted=%t", allowed, nativeErr, trusted)
		}
		_, err = resolveBaseSource(ctx, nil, "dir:signed", platform, store, system, root, "")
		if (err == nil) != trusted {
			t.Fatalf("Coopr signature verification error = %v, want trusted=%t", err, trusted)
		}
	}
}

func TestFilesystemSourcePolicyUsesConfinedSymlinkTarget(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir()
	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	actual := filepath.Join(root, strings.TrimPrefix(outside, string(filepath.Separator)))
	var expected v1.Descriptor
	for _, fixture := range []struct{ path, marker string }{{outside, "host"}, {actual, "context"}} {
		source, err := orasoci.NewWithContext(ctx, fixture.path)
		if err != nil {
			t.Fatal(err)
		}
		manifest, _ := sourceTestImage(t, ctx, source, platform, fixture.marker)
		if err := source.Tag(ctx, manifest, "latest"); err != nil {
			t.Fatal(err)
		}
		if fixture.path == actual {
			expected = manifest
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, allowOutside := range []bool{true, false} {
		defaultRequirement, outsideRequirement := "insecureAcceptAnything", "reject"
		if allowOutside {
			defaultRequirement, outsideRequirement = outsideRequirement, defaultRequirement
		}
		data, err := json.Marshal(map[string]any{
			"default":    []map[string]string{{"type": defaultRequirement}},
			"transports": map[string]any{"oci": map[string]any{outside: []map[string]string{{"type": outsideRequirement}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		policyFile := filepath.Join(root, "policy.json")
		if err := os.WriteFile(policyFile, data, 0o600); err != nil {
			t.Fatal(err)
		}
		system := &types.SystemContext{SignaturePolicyPath: policyFile}
		reference, err := alltransports.ParseImageName("oci:" + actual + ":latest")
		if err != nil {
			t.Fatal(err)
		}
		policy, err := signature.DefaultPolicy(system)
		if err != nil {
			t.Fatal(err)
		}
		nativePolicy, err := signature.NewPolicyContext(policy)
		if err != nil {
			t.Fatal(err)
		}
		source, err := reference.NewImageSource(ctx, system)
		if err != nil {
			t.Fatal(err)
		}
		allowed, nativeErr := nativePolicy.IsRunningImageAllowed(ctx, image.UnparsedInstance(source, nil))
		if err := source.Close(); err != nil {
			t.Fatal(err)
		}
		if err := nativePolicy.Destroy(); err != nil {
			t.Fatal(err)
		}
		if allowed == allowOutside || (nativeErr == nil) == allowOutside {
			t.Fatalf("native confined source policy = %t, %v, outside allowed=%t", allowed, nativeErr, allowOutside)
		}
		storeRoot := t.TempDir()
		store, err := storage.GetStore(storage.StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(storeRoot, "graph"), RunRoot: filepath.Join(storeRoot, "run")})
		if err != nil {
			t.Fatal(err)
		}
		selected, resolveErr := resolveBaseSource(ctx, nil, "oci:link:latest", platform, store, system, root, "")
		_, closeErr := store.Shutdown(true)
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if (resolveErr == nil) != allowed {
			t.Fatalf("Coopr policy error = %v, native confined source allowed=%t", resolveErr, allowed)
		}
		if allowed && selected.Selected.Digest != expected.Digest {
			t.Fatalf("selected manifest = %s, want confined source %s", selected.Selected.Digest, expected.Digest)
		}
	}
}
