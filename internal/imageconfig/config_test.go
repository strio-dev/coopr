package imageconfig

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"coopr/internal/definition"
	"github.com/opencontainers/go-digest"
)

func instruction(name string, args []string, props map[string]string) definition.Instruction {
	return definition.Instruction{Name: name, Arguments: args, Properties: props}
}

func TestDockerConfigFieldsRoundTripAndAccessors(t *testing.T) {
	config, err := Parse([]byte(`{
  "config": {
    "Healthcheck": {"Test":["CMD-SHELL","true"],"Interval":30000000000,"Timeout":3000000000,"StartPeriod":5000000000,"StartInterval":1000000000,"Retries":4,"vendor":{"keep":true}},
    "OnBuild":["RUN make generated"],
    "vendorNested":{"keep":true}
  }
}`))
	if err != nil {
		t.Fatal(err)
	}
	healthcheck, err := config.Healthcheck()
	if err != nil {
		t.Fatal(err)
	}
	wantHealthcheck := &Healthcheck{
		Test: []string{"CMD-SHELL", "true"}, Interval: 30 * time.Second, Timeout: 3 * time.Second,
		StartPeriod: 5 * time.Second, StartInterval: time.Second, Retries: 4,
	}
	if !reflect.DeepEqual(healthcheck, wantHealthcheck) {
		t.Fatalf("Healthcheck() = %#v, want %#v", healthcheck, wantHealthcheck)
	}
	onBuild, err := config.OnBuild()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(onBuild, []string{"RUN make generated"}) {
		t.Fatalf("OnBuild() = %#v", onBuild)
	}
	healthcheck.Test[0] = "mutated"
	onBuild[0] = "mutated"
	storedHealthcheck, _ := config.Healthcheck()
	storedOnBuild, _ := config.OnBuild()
	if storedHealthcheck.Test[0] != "CMD-SHELL" || storedOnBuild[0] != "RUN make generated" {
		t.Fatal("Docker config accessors returned aliased slices")
	}
	raw, err := config.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"Healthcheck"`, `"OnBuild"`, `"vendor":{"keep":true}`, `"vendorNested":{"keep":true}`} {
		if !strings.Contains(string(raw), fragment) {
			t.Fatalf("round trip lost %s: %s", fragment, raw)
		}
	}
}

func TestApplyHealthcheckAndOnBuild(t *testing.T) {
	config, err := Parse([]byte(`{"config":{"OnBuild":["RUN inherited"],"vendorNested":{"keep":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Apply(instruction("healthcheck", []string{"CMD", "/bin/check", "--ready"}, map[string]string{
		"interval": "45s", "timeout": "2s", "start-period": "4s", "start-interval": "500ms", "retries": "5",
	})); err != nil {
		t.Fatal(err)
	}
	if err := config.Apply(instruction("onbuild", []string{"COPY generated /generated"}, nil)); err != nil {
		t.Fatal(err)
	}
	healthcheck, err := config.Healthcheck()
	if err != nil {
		t.Fatal(err)
	}
	want := &Healthcheck{
		Test: []string{"CMD", "/bin/check", "--ready"}, Interval: 45 * time.Second, Timeout: 2 * time.Second,
		StartPeriod: 4 * time.Second, StartInterval: 500 * time.Millisecond, Retries: 5,
	}
	if !reflect.DeepEqual(healthcheck, want) {
		t.Fatalf("Healthcheck() = %#v, want %#v", healthcheck, want)
	}
	onBuild, err := config.OnBuild()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(onBuild, []string{"RUN inherited", "COPY generated /generated"}) {
		t.Fatalf("OnBuild() = %#v", onBuild)
	}

	child := config.Clone()
	if err := child.ClearOnBuild(); err != nil {
		t.Fatal(err)
	}
	childOnBuild, err := child.OnBuild()
	if err != nil {
		t.Fatal(err)
	}
	parentOnBuild, err := config.OnBuild()
	if err != nil {
		t.Fatal(err)
	}
	if len(childOnBuild) != 0 || len(parentOnBuild) != 2 {
		t.Fatalf("child OnBuild=%v parent OnBuild=%v", childOnBuild, parentOnBuild)
	}
	if _, exists := nested(t, child)["vendorNested"]; !exists {
		t.Fatal("ClearOnBuild discarded nested extension")
	}
}

func TestHealthcheckNoneAndMalformedDockerConfig(t *testing.T) {
	config := New()
	if err := config.Apply(instruction("healthcheck", []string{"NONE"}, nil)); err != nil {
		t.Fatal(err)
	}
	healthcheck, err := config.Healthcheck()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(healthcheck, &Healthcheck{Test: []string{"NONE"}}) {
		t.Fatalf("Healthcheck() = %#v", healthcheck)
	}
	tests := []struct{ raw, want string }{
		{`{"config":{"Healthcheck":[]}}`, "config.Healthcheck"},
		{`{"config":{"Healthcheck":{"Test":["NONE"],"Test":["NONE"]}}}`, `duplicate key "Test"`},
		{`{"config":{"Healthcheck":{"test":["NONE"]}}}`, `must be spelled "Test"`},
		{`{"config":{"Healthcheck":{"Test":[]}}}`, "test"},
		{`{"config":{"Healthcheck":{"Test":["OTHER","true"]}}}`, "CMD, CMD-SHELL, or NONE"},
		{`{"config":{"Healthcheck":{"Test":["CMD-SHELL","one","two"]}}}`, "exactly one command"},
		{`{"config":{"Healthcheck":{"Test":["NONE"],"Interval":1}}}`, "NONE"},
		{`{"config":{"Healthcheck":{"Test":["CMD","true"],"Interval":-1}}}`, "Interval"},
		{`{"config":{"Healthcheck":{"Test":["CMD","true"],"Retries":-1}}}`, "retries"},
		{`{"config":{"OnBuild":"RUN true"}}`, "config.OnBuild"},
		{"{\"config\":{\"OnBuild\":[\"RUN true\\u0000\"]}}", "without NUL"},
	}
	for _, test := range tests {
		_, err := Parse([]byte(test.raw))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("Parse(%s) error = %v, want containing %q", test.raw, err, test.want)
		}
	}

	heredoc, err := Parse([]byte("{\"config\":{\"OnBuild\":[\"RUN <<EOF\\necho inherited > /marker\\nEOF\"]}}"))
	if err != nil {
		t.Fatalf("Parse heredoc ONBUILD: %v", err)
	}
	onBuild, err := heredoc.OnBuild()
	if err != nil || !reflect.DeepEqual(onBuild, []string{"RUN <<EOF\necho inherited > /marker\nEOF"}) {
		t.Fatalf("OnBuild heredoc = %#v, %v", onBuild, err)
	}
}

func snapshot(t *testing.T, config *Config) map[string]json.RawMessage {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func nested(t *testing.T, config *Config) map[string]json.RawMessage {
	t.Helper()
	var value map[string]json.RawMessage
	if err := json.Unmarshal(snapshot(t, config)["config"], &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func checkField(t *testing.T, object map[string]json.RawMessage, key string, want any) {
	t.Helper()
	var got any
	if err := json.Unmarshal(object[key], &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %#v, want %#v", key, got, want)
	}
}

func TestInheritanceAndFork(t *testing.T) {
	base, err := Parse([]byte(`{"architecture":"amd64","os":"linux","history":[{"created_by":"base"}],"x-custom":{"number":7},"config":{"Env":["A=old","B=keep","A=duplicate"],"Labels":{"owner":"base"},"WorkingDir":"/opt/app","StopSignal":"SIGTERM","Volumes":{"/data":{}},"ExposedPorts":{"8080/tcp":{}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	fork := base.Clone()
	if err := fork.Apply(instruction("env", nil, map[string]string{"A": "new", "C": "added"})); err != nil {
		t.Fatal(err)
	}
	if err := fork.Apply(instruction("label", nil, map[string]string{"owner": "fork"})); err != nil {
		t.Fatal(err)
	}
	checkField(t, nested(t, base), "Env", []any{"A=old", "B=keep", "A=duplicate"})
	checkField(t, nested(t, base), "Labels", map[string]any{"owner": "base"})
	checkField(t, nested(t, fork), "Env", []any{"A=new", "B=keep", "C=added"})
	checkField(t, nested(t, fork), "Labels", map[string]any{"owner": "fork"})
	for _, key := range []string{"StopSignal", "Volumes", "ExposedPorts"} {
		if !reflect.DeepEqual(nested(t, base)[key], nested(t, fork)[key]) {
			t.Fatalf("lost inherited config field %s", key)
		}
	}
	for _, key := range []string{"architecture", "os", "history", "x-custom"} {
		if !reflect.DeepEqual(snapshot(t, base)[key], snapshot(t, fork)[key]) {
			t.Fatalf("lost inherited image field %s", key)
		}
	}
}

func TestWorkdirAndExecVectors(t *testing.T) {
	config, err := Parse([]byte(`{"config":{"WorkingDir":"/srv/app","Cmd":["old"],"Entrypoint":["/old"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range []definition.Instruction{
		instruction("workdir", []string{"../logs"}, nil),
		instruction("user", []string{"1000:1000"}, nil),
		instruction("entrypoint", []string{"/bin/app", "--literal space"}, nil),
		instruction("arg", []string{"build_only", "yes"}, nil),
	} {
		if err := config.Apply(inst); err != nil {
			t.Fatal(err)
		}
	}
	value := nested(t, config)
	checkField(t, value, "WorkingDir", "/srv/logs")
	checkField(t, value, "User", "1000:1000")
	checkField(t, value, "Entrypoint", []any{"/bin/app", "--literal space"})
	if _, ok := value["Cmd"]; ok {
		t.Fatal("inherited command was not cleared by entrypoint")
	}
	if _, ok := value["build_only"]; ok {
		t.Fatal("arg leaked into image configuration")
	}
	if err := config.Apply(instruction("cmd", []string{"serve", "*.txt"}, nil)); err != nil {
		t.Fatal(err)
	}
	if err := config.Apply(instruction("entrypoint", []string{"/bin/other"}, nil)); err != nil {
		t.Fatal(err)
	}
	checkField(t, nested(t, config), "Cmd", []any{"serve", "*.txt"})
	if err := config.Apply(instruction("cmd", []string{"local"}, nil)); err != nil {
		t.Fatal(err)
	}
	if err := config.Apply(instruction("entrypoint", []string{"/bin/latest"}, nil)); err != nil {
		t.Fatal(err)
	}
	checkField(t, nested(t, config), "Cmd", []any{"local"})
	fork := config.Clone()
	if err := fork.Apply(instruction("entrypoint", []string{"/bin/fork"}, nil)); err != nil {
		t.Fatal(err)
	}
	if _, ok := nested(t, fork)["Cmd"]; ok {
		t.Fatal("fork inherited local command state")
	}
	checkField(t, nested(t, config), "Cmd", []any{"local"})
}

func TestPositionalEnvAndInvalidConfig(t *testing.T) {
	config := New()
	if err := config.Apply(instruction("env", []string{"PORT", "8080"}, nil)); err != nil {
		t.Fatal(err)
	}
	checkField(t, nested(t, config), "Env", []any{"PORT=8080"})
	if _, err := Parse([]byte(`{"config":[]}`)); err == nil {
		t.Fatal("accepted non-object config")
	}
	if err := config.Apply(instruction("copy", []string{"a", "b"}, nil)); err == nil {
		t.Fatal("accepted filesystem instruction as config change")
	}
	withNull, err := Parse([]byte(`{"config":{"Labels":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := withNull.Apply(instruction("label", nil, map[string]string{"a": "b"})); err != nil {
		t.Fatal(err)
	}
	checkField(t, nested(t, withNull), "Labels", map[string]any{"a": "b"})
}

func TestParseRejectsAmbiguousOrUnsafeOCIConfig(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"duplicate top-level standard key", `{"config":{},"config":{}}`, `duplicate key "config"`},
		{"mis-cased top-level standard key", `{"Config":{}}`, `must be spelled "config"`},
		{"duplicate nested standard key", `{"config":{"Env":[],"Env":[]}}`, `duplicate key "Env"`},
		{"mis-cased nested standard key", `{"config":{"env":[]}}`, `must be spelled "Env"`},
		{"unknown rootfs field", `{"rootfs":{"type":"layers","diff_ids":[],"vendor":true}}`, `unsupported field "vendor"`},
		{"unknown history field", `{"history":[{"vendor":true}]}`, `unsupported field "vendor"`},
		{"rootfs is not object", `{"rootfs":[]}`, `rootfs must be an object`},
		{"history is not array", `{"history":{}}`, `history must be an array`},
		{"history entry is not object", `{"history":[null]}`, `history[0] must be an object`},
		{"invalid rootfs type", `{"rootfs":{"type":"other","diff_ids":[]}}`, `rootfs type`},
		{"invalid diff id", `{"rootfs":{"type":"layers","diff_ids":["not-a-digest"]}}`, `rootfs diff_ids`},
		{"partial platform", `{"architecture":"amd64"}`, `architecture and os must either both be set`},
		{"invalid platform token", `{"architecture":"amd64","os":"linux bad"}`, `invalid os`},
		{"malformed known config field", `{"config":{"Env":{}}}`, `config.Env`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.raw))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestApplyAllNormalizedMetadataAndShell(t *testing.T) {
	config, err := Parse([]byte(`{"config":{"Shell":["/bin/inherited","-c"],"vendorNested":{"keep":true}},"vendorTop":"keep"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range []definition.Instruction{
		instruction("shell", []string{"/bin/bash", "-euxo", "pipefail", "-c"}, nil),
		instruction("stopsignal", []string{"SIGTERM"}, nil),
		instruction("expose", []string{"8080/tcp"}, nil),
		instruction("expose", []string{"8080/tcp"}, nil),
		instruction("volume", []string{"/data"}, nil),
		instruction("maintainer", []string{"Coopr Maintainers"}, nil),
	} {
		if err := config.Apply(inst); err != nil {
			t.Fatalf("Apply(%s): %v", inst.Name, err)
		}
	}
	shell, err := config.Shell([]string{"ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(shell, []string{"/bin/bash", "-euxo", "pipefail", "-c"}) {
		t.Fatalf("Shell() = %v", shell)
	}
	shell[0] = "mutated"
	checkField(t, nested(t, config), "Shell", []any{"/bin/bash", "-euxo", "pipefail", "-c"})
	checkField(t, nested(t, config), "StopSignal", "SIGTERM")
	checkField(t, nested(t, config), "ExposedPorts", map[string]any{"8080/tcp": map[string]any{}})
	checkField(t, nested(t, config), "Volumes", map[string]any{"/data": map[string]any{}})
	checkField(t, snapshot(t, config), "author", "Coopr Maintainers")
	checkField(t, snapshot(t, config), "vendorTop", "keep")

	empty := New()
	fallback := []string{"/bin/sh", "-c"}
	got, err := empty.Shell(fallback)
	if err != nil {
		t.Fatal(err)
	}
	got[0] = "changed"
	if fallback[0] != "/bin/sh" {
		t.Fatal("Shell returned the caller's fallback slice")
	}
}

func TestAdoptExecutorProvenancePreservesRawFieldsAndLocalCmdState(t *testing.T) {
	config, err := Parse([]byte(`{"architecture":"amd64","os":"linux","vendorTop":{"keep":true},"config":{"Cmd":["inherited"],"vendorNested":{"keep":true}},"rootfs":{"type":"layers","diff_ids":[]},"history":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Apply(instruction("cmd", []string{"local"}, nil)); err != nil {
		t.Fatal(err)
	}
	emitted := []byte(`{"created":"2026-09-27T12:00:00Z","architecture":"amd64","os":"linux","config":{"Cmd":["executor-copy"]},"rootfs":{"type":"layers","diff_ids":["sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]},"history":[{"created_by":"RUN true"}]}`)
	if err := config.AdoptExecutorProvenance(emitted); err != nil {
		t.Fatal(err)
	}
	if err := config.Apply(instruction("entrypoint", []string{"/bin/app"}, nil)); err != nil {
		t.Fatal(err)
	}
	checkField(t, nested(t, config), "Cmd", []any{"local"})
	checkField(t, nested(t, config), "vendorNested", map[string]any{"keep": true})
	checkField(t, snapshot(t, config), "vendorTop", map[string]any{"keep": true})
	checkField(t, snapshot(t, config), "created", "2026-09-27T12:00:00Z")
	var rootfs map[string]any
	if err := json.Unmarshal(snapshot(t, config)["rootfs"], &rootfs); err != nil {
		t.Fatal(err)
	}
	if len(rootfs["diff_ids"].([]any)) != 1 {
		t.Fatalf("rootfs = %#v", rootfs)
	}

	before, _ := json.Marshal(config)
	if err := config.AdoptExecutorProvenance([]byte(`{"architecture":"arm64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)); err == nil {
		t.Fatal("accepted mismatched executor platform")
	}
	after, _ := json.Marshal(config)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed provenance adoption mutated config")
	}
}

func TestAdoptExecutorProvenanceInitializesScratchPlatform(t *testing.T) {
	config := New()
	if err := config.AdoptExecutorProvenance([]byte(`{"architecture":"arm64","variant":"v8","os":"linux","rootfs":{"type":"layers","diff_ids":[]},"history":[]}`)); err != nil {
		t.Fatal(err)
	}
	checkField(t, snapshot(t, config), "architecture", "arm64")
	checkField(t, snapshot(t, config), "variant", "v8")
	checkField(t, snapshot(t, config), "os", "linux")
}

func TestFlattenPackagePreservesSafeExtensions(t *testing.T) {
	config, err := Parse([]byte(`{"architecture":"amd64","os":"linux","vendorTop":7,"config":{"Env":["A=B"],"vendorNested":true},"rootfs":{"type":"layers","diff_ids":["sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"]},"history":[{"created_by":"base"},{"created_by":"LABEL a=b","empty_layer":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	layer := digest.FromString("flattened package")
	created := time.Unix(123, 0).UTC()
	if err := config.FlattenPackage(layer, &created); err != nil {
		t.Fatal(err)
	}
	var image struct {
		RootFS struct {
			Type    string          `json:"type"`
			DiffIDs []digest.Digest `json:"diff_ids"`
		} `json:"rootfs"`
		History []struct {
			Created    *time.Time `json:"created"`
			CreatedBy  string     `json:"created_by"`
			EmptyLayer bool       `json:"empty_layer"`
		} `json:"history"`
	}
	raw, _ := json.Marshal(config)
	if err := json.Unmarshal(raw, &image); err != nil {
		t.Fatal(err)
	}
	if image.RootFS.Type != "layers" || !reflect.DeepEqual(image.RootFS.DiffIDs, []digest.Digest{layer}) {
		t.Fatalf("rootfs = %+v", image.RootFS)
	}
	if len(image.History) != 3 || !image.History[0].EmptyLayer || !image.History[1].EmptyLayer || image.History[2].EmptyLayer || image.History[2].CreatedBy != "coopr package snapshot" {
		t.Fatalf("history = %+v", image.History)
	}
	if image.History[2].Created == nil || !image.History[2].Created.Equal(created) {
		t.Fatalf("snapshot history created = %v, want %v", image.History[2].Created, created)
	}
	checkField(t, snapshot(t, config), "vendorTop", float64(7))
	checkField(t, nested(t, config), "vendorNested", true)
	if err := config.FlattenPackage("bad", nil); err == nil {
		t.Fatal("accepted invalid flattened layer digest")
	}
}
