package buildah

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"coopr/internal/definition"
	"coopr/internal/planner"
)

func TestCredentialRunMountsUseMetadataOnlyInstructionCacheKeys(t *testing.T) {
	for _, mountType := range []string{"secret", "ssh"} {
		t.Run(mountType, func(t *testing.T) {
			properties := map[string]string{"id": "credential", "target": "/run/credential", "required": "false", "mode": "0400", "uid": "1", "gid": "2"}
			mount := definition.Instruction{Name: "mount", Arguments: []string{mountType}, Properties: properties}
			operation := planner.Operation{Instruction: definition.Instruction{
				Name: "run", Form: "shell", Arguments: []string{"cat /run/secrets/credential"}, Children: []definition.Instruction{mount},
			}}
			input := instructionCacheTestInput(operation)
			input.ResolvedInputsComplete = true
			input.ResolvedInputs = []instructionCacheResolvedInput{{Kind: mountType, Identity: "credential"}}
			key, cacheable, err := instructionCacheKey(input)
			if err != nil || !cacheable || key == "" {
				t.Fatalf("credential RUN cache key = %q, cacheable = %v, error = %v", key, cacheable, err)
			}

			changes := map[string]string{"id": "other", "target": "/run/other", "required": "true", "mode": "0440", "uid": "3", "gid": "4"}
			for property, value := range changes {
				t.Run(property, func(t *testing.T) {
					changedProperties := make(map[string]string, len(properties))
					for name, value := range properties {
						changedProperties[name] = value
					}
					changedProperties[property] = value
					changed := input
					changed.Operation = operation
					changed.Operation.Children = []definition.Instruction{{Name: "mount", Arguments: []string{mountType}, Properties: changedProperties}}
					changedKey, changedCacheable, err := instructionCacheKey(changed)
					if err != nil || !changedCacheable {
						t.Fatalf("changed credential RUN cache key = %q, cacheable = %v, error = %v", changedKey, changedCacheable, err)
					}
					if changedKey == key {
						t.Fatalf("credential mount %s did not invalidate the RUN cache key", property)
					}
				})
			}
		})
	}
}

func TestPrepareRunInputCredentialIdentityExcludesRuntimeSource(t *testing.T) {
	for _, mountType := range []string{"secret", "ssh"} {
		t.Run(mountType, func(t *testing.T) {
			prepared, err := prepareRunInput(context.Background(), &inspectingRunBuilder{}, "", nil, Run{
				Command: []string{"/bin/true"},
				Mounts:  []RunMount{{Type: mountType, Properties: map[string]string{"id": "credential", "target": "/run/credential", "required": "true"}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !prepared.complete {
				t.Fatal("credential mount made resolved RUN inputs incomplete")
			}
			if len(prepared.resolved) != 1 || prepared.resolved[0] != (instructionCacheResolvedInput{Kind: mountType, Identity: "credential"}) {
				t.Fatalf("resolved credential input = %#v", prepared.resolved)
			}
		})
	}
}

func TestSecretValueChangeReusesInstructionCache(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf("from %q\nrun \"cat /run/secrets/token >/proof\" network=\"none\" { mount \"secret\" id=\"token\" required=\"true\" }\n", base.reference))
	build := func(name, value string) string {
		t.Helper()
		secretPath := filepath.Join(root, name+"-token")
		if err := os.WriteFile(secretPath, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		layout := filepath.Join(root, name)
		_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output:  Output{Path: layout},
			Secrets: []string{"id=token,src=" + secretPath}, SignaturePolicyPath: policy,
			Stdout: io.Discard, Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		last := manifest.Layers[len(manifest.Layers)-1]
		return readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof")
	}
	first := build("first", "first\n")
	if records := instructionCacheRecordCount(t, store); records != 1 {
		t.Fatalf("cold secret RUN cache records = %d, want 1", records)
	}
	second := build("second", "second\n")
	if first != "first\n" || second != first {
		t.Fatalf("secret cache output: first=%q second=%q", first, second)
	}
	if records := instructionCacheRecordCount(t, store); records != 1 {
		t.Fatalf("warm secret RUN cache records = %d, want 1", records)
	}
	warmWithoutSource := filepath.Join(root, "without-source")
	if _, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
		Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
		Output:              Output{Path: warmWithoutSource},
		SignaturePolicyPath: policy, Stdout: io.Discard, Stderr: io.Discard,
	}); err != nil {
		t.Fatalf("warm required-secret cache hit without runtime source: %v", err)
	}
	manifest, _ := readPlanImage(t, warmWithoutSource)
	last := manifest.Layers[len(manifest.Layers)-1]
	if proof := readLayerFile(t, filepath.Join(warmWithoutSource, "blobs", "sha256", last.Digest.Encoded()), "proof"); proof != first {
		t.Fatalf("warm required-secret cache output without source = %q, want %q", proof, first)
	}
}

func TestSSHAgentChangeReusesInstructionCache(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	store := cacheTestStore(root)
	base := newLiveBusyBoxStorage(t, ctx, root, store)
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, fmt.Sprintf("from %q\nrun \"test -S /run/buildkit/ssh_agent.0 && od -An -N16 -tx1 /dev/urandom | tr -d ' \\\\n' >/proof\" network=\"none\" { mount \"ssh\" id=\"default\" required=\"true\" }\n", base.reference))
	build := func(name string) string {
		t.Helper()
		listener, err := net.Listen("unix", filepath.Join(root, name+".sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = listener.Close() }()
		layout := filepath.Join(root, name)
		if _, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
			Output: Output{Path: layout},
			SSH:    []string{"default=" + listener.Addr().String()}, SignaturePolicyPath: policy,
			Stdout: io.Discard, Stderr: io.Discard,
		}); err != nil {
			t.Fatal(err)
		}
		manifest, _ := readPlanImage(t, layout)
		last := manifest.Layers[len(manifest.Layers)-1]
		return readLayerFile(t, filepath.Join(layout, "blobs", "sha256", last.Digest.Encoded()), "proof")
	}
	first := build("first")
	second := build("second")
	if first == "" || second != first {
		t.Fatalf("SSH cache output: first=%q second=%q", first, second)
	}
	if records := instructionCacheRecordCount(t, store); records != 1 {
		t.Fatalf("SSH RUN cache records = %d, want 1", records)
	}
}
