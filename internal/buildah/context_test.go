package buildah

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	upstream "go.podman.io/buildah"
	"go.podman.io/storage/pkg/fileutils"
)

func TestContextExplicitIgnoreReplacesDefault(t *testing.T) {
	root := t.TempDir()
	selected := filepath.Join(root, "explicit.ignore")
	for name, data := range map[string]string{".dockerignore": "default-only\n", "explicit.ignore": "specific-only\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := prepareContextPolicyWithIgnore(root, nil, selected)
	if err != nil {
		t.Fatal(err)
	}
	options, err := policy.apply(upstream.AddAndCopyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Excludes) != 1 || options.Excludes[0] != "specific-only" || options.IgnoreFile != selected {
		t.Fatalf("explicit policy = %+v", options)
	}
}

func TestPreparedContextMissingExcludedSourceKeepsPolicyDiagnostic(t *testing.T) {
	root := t.TempDir()
	policy := contextPolicy{directory: root, authoredExcludes: []string{"protected", "ignored", "!ignored/keep"}}
	for _, source := range []string{"protected", "./protected", "/protected", "../protected", "ignored/hidden"} {
		if _, _, err := policy.applyLocalCopy(upstream.AddAndCopyOptions{}, []string{source}); err == nil || !strings.Contains(err.Error(), "filtered out") {
			t.Errorf("source %q error=%v", source, err)
		}
	}
	for _, source := range []string{"missing", "ignored/keep"} {
		if _, _, err := policy.applyLocalCopy(upstream.AddAndCopyOptions{}, []string{source}); err != nil {
			t.Errorf("source %q was incorrectly classified as excluded: %v", source, err)
		}
	}
}

func TestPrepareContextPolicyMergesDockerignoreInstructionAndArtifacts(t *testing.T) {
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), []byte("\ufeff# comment\n /ignored \n*.log\n!keep.log\n!coopr-cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(contextDir, "coopr-cache")
	if err := os.Mkdir(artifact, 0o700); err != nil {
		t.Fatal(err)
	}

	policy, err := prepareContextPolicy(contextDir, []string{artifact})
	if err != nil {
		t.Fatal(err)
	}
	options, err := policy.apply(upstream.AddAndCopyOptions{Excludes: []string{"*.tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"\ufeff# comment", " /ignored ", "*.log", "!keep.log", "!coopr-cache", "*.tmp", "coopr-cache"}
	if !reflect.DeepEqual(options.Excludes, want) {
		t.Fatalf("excludes = %#v, want %#v", options.Excludes, want)
	}
	if options.ContextDir != contextDir || options.IgnoreFile != filepath.Join(contextDir, ".dockerignore") {
		t.Fatalf("context options = %+v", options)
	}
	excluded, err := fileutils.Matches("coopr-cache/blob", options.Excludes)
	if err != nil {
		t.Fatal(err)
	}
	if !excluded {
		t.Fatal("authored negation reopened protected context artifact")
	}
}

func TestPrepareContextPolicyProtectsArtifactAliasesAndSpecialNames(t *testing.T) {
	contextDir := t.TempDir()
	protectedNames := []string{"cache[1]", " cache ", "cache+(1){x}^$|", "#cache", "!cache", "\tcache\u00a0", `cache*?\[a]`}
	artifacts := make([]string, 0, len(protectedNames))
	for _, name := range protectedNames {
		artifact := filepath.Join(contextDir, name)
		if err := os.Mkdir(artifact, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(artifact, "private"), []byte("protected"), 0600); err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, artifact)
	}
	artifact := artifacts[0]
	neighbors := []string{"cache", "cache1", "cacheee1x", "public"}
	for _, name := range neighbors {
		if err := os.WriteFile(filepath.Join(contextDir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(artifact, filepath.Join(contextDir, "cache-alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "selected"), []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), []byte("ignored-first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "active")
	if err := os.Symlink(contextDir, root); err != nil {
		t.Fatal(err)
	}
	policy, err := prepareContextPolicy(root, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	next := t.TempDir()
	if err := os.WriteFile(filepath.Join(next, "selected"), []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(next, ".dockerignore"), []byte("ignored-second\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(next, root+"-next"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root+"-next", root); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"first", "second"} {
		options, err := policy.apply(upstream.AddAndCopyOptions{})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, cleanup, err := snapshotContext(t.Context(), options.ContextDir, options.Excludes, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := cleanup(); err != nil {
				t.Error(err)
			}
		})
		if data, err := os.ReadFile(filepath.Join(snapshot, "selected")); err != nil || string(data) != want {
			t.Fatalf("policy root payload=%q err=%v want=%q", data, err, want)
		}
		if data, err := os.ReadFile(options.IgnoreFile); err != nil || string(data) != "ignored-"+want+"\n" {
			t.Fatalf("policy ignore content=%q err=%v", data, err)
		}
		if want == "first" {
			prepared, captured, err := captureBuildContext(t.Context(), PlanOptions{ContextDir: options.ContextDir, ContextArtifacts: artifacts}, true)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := captured.cleanup(); err != nil {
					t.Error(err)
				}
			})
			roundtrip, err := prepareContextPolicyWithIgnore(prepared.ContextDir, nil, prepared.IgnoreFile)
			if err != nil {
				t.Fatal(err)
			}
			matcher, err := fileutils.NewPatternMatcher(roundtrip.authoredExcludes)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range append(protectedNames, "cache-alias") {
				if _, err := os.Lstat(filepath.Join(snapshot, name)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("protected artifact %q survived capture: %v", name, err)
				}
				if excluded, err := matcher.IsMatch(name + "/private"); err != nil || !excluded {
					t.Errorf("serialized policy lost artifact %q: excluded=%t err=%v", name, excluded, err)
				}
			}
			for _, name := range neighbors {
				if got, err := os.ReadFile(filepath.Join(snapshot, name)); err != nil || string(got) != name {
					t.Errorf("unprotected neighbor %q changed: %q %v", name, got, err)
				}
				if excluded, err := matcher.IsMatch(name); err != nil || excluded {
					t.Errorf("serialized policy excluded neighbor %q: excluded=%t err=%v", name, excluded, err)
				}
			}
			policy, err = prepareContextPolicy(root, artifacts)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestPrepareContextPolicyRejectsUnsafeOrUnsupportedPolicy(t *testing.T) {
	for _, name := range []string{"cache\nprivate", "cache\xff"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			_, err := prepareContextPolicy(root, []string{filepath.Join(root, name)})
			if err == nil || !strings.Contains(err.Error(), "literal context exclusion") {
				t.Fatalf("unrepresentable protected path error = %v", err)
			}
		})
	}

	t.Run("artifact covers context", func(t *testing.T) {
		parent := t.TempDir()
		contextDir := filepath.Join(parent, "context")
		if err := os.Mkdir(contextDir, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := prepareContextPolicy(contextDir, []string{parent})
		if err == nil || !strings.Contains(err.Error(), "covers build context") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("invalid dockerignore", func(t *testing.T) {
		contextDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), []byte("[\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		policy, err := prepareContextPolicy(contextDir, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = policy.apply(upstream.AddAndCopyOptions{})
		if err == nil || !strings.Contains(err.Error(), "validate context excludes") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("invalid instruction exclude", func(t *testing.T) {
		contextDir := t.TempDir()
		policy, err := prepareContextPolicy(contextDir, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = policy.apply(upstream.AddAndCopyOptions{Excludes: []string{"["}})
		if err == nil || !strings.Contains(err.Error(), "validate COPY/ADD excludes") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestContextPolicyKeepsDockerignoreAndCopyExcludeNegationsIndependent(t *testing.T) {
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), []byte("secret\n*.log\n!keep.log\ntree/*\n!tree/keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(contextDir, "tree"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"secret", "keep.log", "drop.txt", "public.txt", "normal"} {
		if err := os.WriteFile(filepath.Join(contextDir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"keep", "drop"} {
		if err := os.WriteFile(filepath.Join(contextDir, "tree", name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := prepareContextPolicy(contextDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	options, err := policy.apply(upstream.AddAndCopyOptions{Excludes: []string{"*.txt", "!public.txt", "!secret", "keep.log"}})
	if err != nil {
		t.Fatal(err)
	}
	if options.IgnoreFile != filepath.Join(contextDir, ".dockerignore") {
		t.Fatalf("ignore file = %q", options.IgnoreFile)
	}
	matcher, err := fileutils.NewPatternMatcher(options.Excludes)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path     string
		excluded bool
	}{
		{path: "secret", excluded: true},   // --exclude negation cannot reopen .dockerignore.
		{path: "keep.log", excluded: true}, // .dockerignore negation cannot reopen --exclude.
		{path: "drop.txt", excluded: true},
		{path: "public.txt", excluded: false}, // Negation reopens an earlier rule in its own layer.
		{path: "normal", excluded: false},
		{path: "tree/keep", excluded: false}, // Nested negations survive the finite-tree reduction.
		{path: "tree/drop", excluded: true},
	} {
		got, err := matcher.IsMatch(test.path)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.excluded {
			t.Errorf("%s excluded = %t, want %t; patterns = %#v", test.path, got, test.excluded, options.Excludes)
		}
	}

	snapshot, cleanup, err := snapshotContext(t.Context(), options.ContextDir, options.Excludes, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	})
	for _, name := range []string{"public.txt", "normal", "tree/keep"} {
		if _, err := os.Stat(filepath.Join(snapshot, name)); err != nil {
			t.Errorf("independent filters omitted %q: %v", name, err)
		}
	}
	for _, name := range []string{"secret", "keep.log", "drop.txt", "tree/drop"} {
		if _, err := os.Stat(filepath.Join(snapshot, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("independent filters exposed %q: %v", name, err)
		}
	}
}

func TestPrepareContextPolicyAllowsMissingDockerignoreAndOutsideArtifacts(t *testing.T) {
	contextDir := t.TempDir()
	policy, err := prepareContextPolicy(contextDir, []string{filepath.Join(t.TempDir(), "output")})
	if err != nil {
		t.Fatal(err)
	}
	options, err := policy.apply(upstream.AddAndCopyOptions{Excludes: []string{"*.tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(options.Excludes, []string{"*.tmp"}) || options.IgnoreFile != "" {
		t.Fatalf("context options = %+v", options)
	}
}

func TestApplyOperationWithContextProtectsArtifactsForLocalCopy(t *testing.T) {
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, ".dockerignore"), []byte("ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(contextDir, "output")
	if err := os.Mkdir(artifact, 0o700); err != nil {
		t.Fatal(err)
	}
	builder := &recordingBuilder{}
	err := applyOperationWithContext(builder, contextDir, []string{artifact}, Copy{
		Sources: []string{"."}, Destination: "/context", Excludes: []string{"*.tmp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "copy:false:" + contextDir + ":/context:::ignored,*.tmp,*.tmp,output:."
	if !reflect.DeepEqual(builder.events, []string{want}) {
		t.Fatalf("events = %#v, want %#v", builder.events, []string{want})
	}
}

func TestApplyOperationWithContextUsesContainerfileCopyPolicy(t *testing.T) {
	contextDir := t.TempDir()
	builder := &recordingBuilder{}
	err := applyOperationWithContext(builder, contextDir, nil, Copy{
		Sources: []string{"first/./nested", "second"}, Destination: "/target/", Parents: true,
		Excludes: []string{"*.tmp", "!keep.tmp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(builder.addOptions) != 1 {
		t.Fatalf("Buildah Add calls = %d, want 1", len(builder.addOptions))
	}
	options := builder.addOptions[0]
	if !options.StripSetuidBit || !options.StripSetgidBit {
		t.Fatalf("local COPY strip options = setuid:%t setgid:%t, want both true", options.StripSetuidBit, options.StripSetgidBit)
	}
	wantExcludes := []string{
		"*.tmp", "!keep.tmp",
		"first/nested/*.tmp", "!first/nested/keep.tmp",
		"second/*.tmp", "!second/keep.tmp",
	}
	if !reflect.DeepEqual(options.Excludes, wantExcludes) {
		t.Fatalf("local COPY excludes = %#v, want %#v", options.Excludes, wantExcludes)
	}
	wantSources := []string{
		filepath.Join(contextDir, "first") + "/./nested",
		"second",
	}
	eventSources := strings.Split(builder.events[0], ":")
	gotSources := strings.Split(eventSources[len(eventSources)-1], ",")
	if !reflect.DeepEqual(gotSources, wantSources) {
		t.Fatalf("local COPY sources = %#v, want %#v", gotSources, wantSources)
	}
}

func TestApplyOperationWithContextLeavesCopyFromOnImageSourcePath(t *testing.T) {
	contextDir := t.TempDir()
	err := applyOperationWithContext(&recordingBuilder{}, contextDir, []string{contextDir}, copyFromImageOperation{})
	if err == nil || !strings.Contains(err.Error(), "COPY --from requires a native Buildah builder") {
		t.Fatalf("COPY --from error = %v", err)
	}
}

func TestContextCooprIgnorePriority(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{".cooprignore", ".containerignore", ".dockerignore"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{".cooprignore", ".containerignore", ".dockerignore"} {
		policy, err := prepareContextPolicy(root, nil)
		if err != nil {
			t.Fatal(err)
		}
		if policy.ignoreFile != filepath.Join(root, name) || !reflect.DeepEqual(policy.authoredExcludes, []string{name}) {
			t.Fatalf("policy = %+v; want only %s", policy, name)
		}
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContextCooprIgnoreSymlinkConfinement(t *testing.T) {
	t.Run("outside", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "private.ignore")
		if err := os.WriteFile(outside, []byte("outside-secret\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, ".cooprignore")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".dockerignore"), []byte("fallback\n"), 0600); err != nil {
			t.Fatal(err)
		}
		patterns, _, err := ReadContextIgnore(root, "")
		if err != nil || !reflect.DeepEqual(patterns, []string{"fallback"}) {
			t.Fatalf("outside symlink policy = %v, %v", patterns, err)
		}
		patterns, _, err = ReadContextIgnore(root, outside)
		if err != nil || !reflect.DeepEqual(patterns, []string{"outside-secret"}) {
			t.Fatalf("explicit outside policy = %v, %v", patterns, err)
		}
	})
	t.Run("inside", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "policy"), []byte("inside\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("policy", filepath.Join(root, ".cooprignore")); err != nil {
			t.Fatal(err)
		}
		patterns, selected, err := ReadContextIgnore(root, "")
		if err != nil || selected != filepath.Join(root, "policy") || !reflect.DeepEqual(patterns, []string{"inside"}) {
			t.Fatalf("inside symlink policy = %v, %q, %v", patterns, selected, err)
		}
	})
	t.Run("directory error", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, ".cooprignore"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".dockerignore"), []byte("fallback\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ReadContextIgnore(root, ""); err == nil {
			t.Fatal("unreadable preferred ignore silently fell back")
		}
	})
}

func TestCooprIgnoreLocalComponentAndPackageIdentity(t *testing.T) {
	root := t.TempDir()
	for name, data := range map[string]string{".cooprignore": "hidden.coopr\nsecret\n", ".dockerignore": "visible.coopr\n", "visible.coopr": "extend", "hidden.coopr": "extend", "secret": "before"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := readLocalComponent(context.Background(), PlanOptions{ContextDir: root}, "./hidden.coopr"); err == nil {
		t.Fatal("read ignored component")
	}
	if _, _, err := readLocalComponent(context.Background(), PlanOptions{ContextDir: root}, "./visible.coopr"); err != nil {
		t.Fatal(err)
	}
	identity := func() string {
		t.Helper()
		_, capture, err := captureBuildContext(t.Context(), PlanOptions{ContextDir: root}, true)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := capture.cleanup(); err != nil {
				t.Error(err)
			}
		}()
		return capture.identity.String()
	}
	before := identity()
	if err := os.WriteFile(filepath.Join(root, "secret"), []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	after := identity()
	if before != after {
		t.Fatal("ignored payload changed package identity")
	}
	if err := os.WriteFile(filepath.Join(root, "visible.coopr"), []byte("extend\n# changed"), 0600); err != nil {
		t.Fatal(err)
	}
	changed := identity()
	if changed == after {
		t.Fatal("visible payload did not change package identity")
	}
}
