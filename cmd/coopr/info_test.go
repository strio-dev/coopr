package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestInfoCommandShape(t *testing.T) {
	command, _, err := newRootCommand().Find([]string{"info"})
	if err != nil || command.Name() != "info" {
		t.Fatalf("missing info: %v", err)
	}
	if command.Flags().Lookup("format") == nil {
		t.Fatal("info lacks --format")
	}
	var out, errs bytes.Buffer
	if status := run([]string{"info", "unexpected"}, &out, &errs); status == 0 {
		t.Fatal("info accepted an argument")
	}
}

func TestInfoReportsConfiguredNativeStore(t *testing.T) {
	options := maintenanceStoreOptions(t.TempDir())
	args := []string{"--root", options.GraphRoot, "--runroot", options.RunRoot, "--storage-driver", "vfs", "info"}
	var out, errs bytes.Buffer
	if status := run(args, &out, &errs); status != 0 {
		t.Fatalf("info status=%d stderr=%s", status, &errs)
	}
	var info map[string]map[string]any
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info["store"]["GraphRoot"] != options.GraphRoot || info["store"]["RunRoot"] != options.RunRoot || info["store"]["GraphDriverName"] != "vfs" {
		t.Fatalf("configured info=%s", &out)
	}
	if info["version"]["coopr"] != version || info["version"]["buildah"] == "" || info["host"]["rootless"] == nil || info["host"]["OCIRuntime"] == nil {
		t.Fatalf("missing native version/host info=%s", &out)
	}
	out.Reset()
	errs.Reset()
	if status := run(append(args, "--format", "{{.store.GraphRoot}}"), &out, &errs); status != 0 || strings.TrimSpace(out.String()) != options.GraphRoot {
		t.Fatalf("info template status=%d stdout=%q stderr=%s", status, &out, &errs)
	}
}
