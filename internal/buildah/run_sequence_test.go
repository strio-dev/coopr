package buildah

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/planner"
	upstream "go.podman.io/buildah"
)

func TestRunSequenceLowersToOneOperation(t *testing.T) {
	plan := testPlan(t, `from "scratch" {
    run {
        exec "/tool" "first"
        mount "tmpfs" target="/scratch"
        exec "/tool" "second" "$literal"
    }
}`)
	request, err := RequestFromPlan(plan, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Operations) != 1 {
		t.Fatalf("lowered %d operations", len(request.Operations))
	}
	run := request.Operations[0].(Run)
	want := [][]string{{"/tool", "first"}, {"/tool", "second", "$literal"}}
	if !reflect.DeepEqual(run.Commands, want) || len(run.Command) != 0 || len(run.Mounts) != 1 {
		t.Fatalf("RUN = %+v", run)
	}
}

type sequenceRecordingBuilder struct {
	recordingBuilder
	inspect func([]string, upstream.RunOptions) error
}

func (b *sequenceRecordingBuilder) run(command []string, options upstream.RunOptions) error {
	if err := b.recordingBuilder.run(command, options); err != nil {
		return err
	}
	return b.inspect(command, options)
}

func TestRunSequenceSharesOneBuildahInvocationAndCleansTemporaryFiles(t *testing.T) {
	prefix := t.TempDir()
	for _, file := range []string{"bin/coopr", "libexec/coopr/" + runSequenceHelper} {
		name := filepath.Join(prefix, file)
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("helper fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", filepath.Join(prefix, "bin"))
	want := [][]string{{"/tool", strings.Repeat("x", 150000)}, {"/tool", "$literal", ""}}
	wantErr := errors.New("first child failed")
	var source string
	builder := &sequenceRecordingBuilder{inspect: func(command []string, options upstream.RunOptions) error {
		if len(command) != 2 || len(options.Mounts) != 1 || len(options.RunMounts) != 1 {
			t.Fatalf("command=%v options=%+v", command, options)
		}
		mount := options.Mounts[0]
		source = mount.Source
		if command[0] != mount.Destination+"/"+runSequenceHelper || command[1] != mount.Destination+"/commands.json" {
			t.Fatalf("helper command=%v mount=%+v", command, mount)
		}
		if !reflect.DeepEqual(mount.Options, []string{"ro", "bind", "exec", "Z"}) {
			t.Fatalf("helper mount options=%v", mount.Options)
		}
		data, err := os.ReadFile(filepath.Join(source, "commands.json"))
		if err != nil {
			t.Fatal(err)
		}
		var got [][]string
		if err := json.Unmarshal(data, &got); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("commands=%v err=%v", got, err)
		}
		for _, file := range []string{source, filepath.Join(source, runSequenceHelper), filepath.Join(source, "commands.json")} {
			info, err := os.Stat(file)
			if err != nil || info.Mode().Perm()&0o004 == 0 {
				t.Fatalf("helper input not readable by target USER: %s %v", file, err)
			}
		}
		if options.User != "1000" || options.WorkingDir != "/work" || !reflect.DeepEqual(options.Env, []string{"VALUE=literal"}) {
			t.Fatalf("RUN user/workdir/environment lost: %+v", options)
		}
		return wantErr
	}}
	err := (Run{Commands: want, User: "1000", WorkingDir: "/work", Env: []string{"VALUE=literal"}, Mounts: []RunMount{{Type: "tmpfs", Properties: map[string]string{"target": "/scratch"}}}}).apply(builder, "")
	if !errors.Is(err, wantErr) || len(builder.events) != 1 {
		t.Fatalf("apply err=%v events=%v", err, builder.events)
	}
	if source == "" {
		t.Fatal("Buildah was not called")
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary helper remains: %v", err)
	}
}

func TestRunSequenceHelperDiscoveryResolvesInstalledSymlink(t *testing.T) {
	prefix := t.TempDir()
	helper := filepath.Join(prefix, "libexec", "coopr", runSequenceHelper)
	for _, name := range []string{filepath.Join(prefix, "bin", "coopr"), helper} {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "coopr")
	if err := os.Symlink(filepath.Join(prefix, "bin", "coopr"), link); err != nil {
		t.Fatal(err)
	}
	got, err := runSequenceHelperBeside(link)
	if err != nil || got != helper {
		t.Fatalf("helper=%q err=%v", got, err)
	}
}

func TestRunSequenceHelperDiscoverySupportsUnpackedArchive(t *testing.T) {
	prefix := t.TempDir()
	executable := filepath.Join(prefix, "coopr")
	helper := filepath.Join(prefix, "libexec", "coopr", runSequenceHelper)
	if err := os.MkdirAll(filepath.Dir(helper), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{executable, helper} {
		if err := os.WriteFile(name, []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, err := findRunSequenceHelperFrom(executable)
	if err != nil || got != helper {
		t.Fatalf("archive helper=%q err=%v", got, err)
	}
}

func TestCaptureRunSequenceOriginDoesNotRequireFilesystem(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "removed-coopr")
	for _, test := range []struct{ executable, argv0, want string }{
		{missing, missing, missing},
		{"/", missing + "-in-a-user-namespace", missing},
		{"/memfd:coopr-in-a-user-namespace", "./removed-coopr-in-a-user-namespace", "./removed-coopr"},
		{"/", "removed-coopr-in-a-user-namespace", "removed-coopr"},
		{"/memfd:coopr", planWorkerName, ""},
	} {
		if got := captureRunSequenceOrigin(test.executable, test.argv0); got != test.want {
			t.Fatalf("capture origin %q/%q = %q, want %q", test.executable, test.argv0, got, test.want)
		}
	}
}

func TestRunSequenceOriginSurvivesUpstreamNamespaceReexec(t *testing.T) {
	prefix := t.TempDir()
	original := filepath.Join(prefix, "coopr")
	if err := os.WriteFile(original, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", prefix)
	t.Chdir(prefix)
	for _, executable := range []string{"/memfd:coopr-in-a-user-namespace", "/"} {
		for _, argv0 := range []string{original + "-in-a-user-namespace", "coopr-in-a-user-namespace", "./coopr-in-a-user-namespace"} {
			got, err := resolveRunSequenceOrigin(executable, argv0)
			if err != nil || got != original {
				t.Fatalf("rootless origin=%q err=%v", got, err)
			}
		}
		if _, err := resolveRunSequenceOrigin(executable, planWorkerName); err == nil {
			t.Fatal("named worker pretended to know the originating installation")
		}
	}
}

func TestRunSequenceHelperDiscoveryDoesNotMaskBrokenInstallation(t *testing.T) {
	alternative := t.TempDir()
	for _, name := range []string{"bin/coopr", "libexec/coopr/" + runSequenceHelper} {
		file := filepath.Join(alternative, name)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", filepath.Join(alternative, "bin"))
	for _, kind := range []string{"missing installed helper", "missing archive helper", "not executable", "broken symlink"} {
		t.Run(kind, func(t *testing.T) {
			prefix := t.TempDir()
			executable := filepath.Join(prefix, "coopr")
			if kind == "missing installed helper" {
				executable = filepath.Join(prefix, "bin", "coopr")
			}
			if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(executable, []byte("fixture"), 0o755); err != nil {
				t.Fatal(err)
			}
			helper := filepath.Join(prefix, "libexec", "coopr", runSequenceHelper)
			if err := os.MkdirAll(filepath.Dir(helper), 0o755); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "not executable":
				if err := os.WriteFile(helper, []byte("fixture"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "broken symlink":
				if err := os.Symlink(filepath.Join(prefix, "missing"), helper); err != nil {
					t.Fatal(err)
				}
			}
			got, err := findRunSequenceHelperFrom(executable)
			if err == nil || got != "" || !strings.Contains(err.Error(), helper) {
				t.Fatalf("invalid installation used another helper: helper=%q err=%v", got, err)
			}
		})
	}
	for _, relative := range []string{"buildah.test", "bin/buildah.test", "go-build12345/b001/exe/coopr", "ab/" + strings.Repeat("ab", 32) + "-d/coopr"} {
		development := filepath.Join(t.TempDir(), relative)
		if err := os.MkdirAll(filepath.Dir(development), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(development, []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := findRunSequenceHelperFrom(development)
		if err != nil || got != filepath.Join(alternative, "libexec", "coopr", runSequenceHelper) {
			t.Fatalf("development helper=%q err=%v", got, err)
		}
	}
}

func TestRunSequenceDevelopmentExecutableLayouts(t *testing.T) {
	for _, executable := range []string{
		"/tmp/coopr", "/usr/local/bin/coopr", "/tmp/release/coopr",
		"/tmp/go-build12345/not-an-object/exe/coopr", "/tmp/release/b001/exe/coopr",
		"/cache/cd/" + strings.Repeat("ab", 32) + "-d/coopr",
		"/cache/zz/" + strings.Repeat("zz", 32) + "-d/coopr",
	} {
		if isGoDevelopmentExecutable(executable) {
			t.Errorf("ordinary installation mistaken for development executable: %s", executable)
		}
	}
}

func TestRunSequenceCacheAndHistoryIncludeEveryCommand(t *testing.T) {
	instruction := definition.Instruction{Name: "run", Commands: []definition.Command{{Form: "exec", Arguments: []string{"/tool", "first"}}, {Form: "exec", Arguments: []string{"/tool", "second"}}}}
	key := func(inst definition.Instruction) string {
		result, cacheable, err := instructionCacheKey(instructionCacheTestInput(planner.Operation{Instruction: inst}))
		if err != nil || !cacheable {
			t.Fatalf("cache key=%q eligible=%v err=%v", result, cacheable, err)
		}
		return result.String()
	}
	original := key(instruction)
	changed := instruction
	changed.Commands = []definition.Command{{Form: "exec", Arguments: []string{"/tool", "first"}}, {Form: "exec", Arguments: []string{"/tool", "changed"}}}
	reversed := instruction
	reversed.Commands = []definition.Command{instruction.Commands[1], instruction.Commands[0]}
	if original == key(changed) || original == key(reversed) || key(changed) == key(reversed) {
		t.Fatal("cache identity omits command content or order")
	}
	history, ok := plannedOperationHistory(planner.Operation{Instruction: instruction}, nil)
	if !ok || history != `RUN exec ["/tool","first"] exec ["/tool","second"]` {
		t.Fatalf("history=%q ok=%v", history, ok)
	}
}
