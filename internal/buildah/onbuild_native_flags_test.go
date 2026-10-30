package buildah

import (
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/planner"
)

func TestOnBuildNativeFlagsExecuteInChildScope(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.write(t, "app/payload", "hello\n")
	def := parseWorkerDefinition(t, fmt.Sprintf(`from %q as="parent" {
    onbuild { arg "network" "none" }
    onbuild { arg "linked" #true }
    onbuild { arg "parents" #true }
    onbuild { arg "unpack" #false }
    onbuild { healthcheck "true" interval="1ns" }
    onbuild { copy "app/payload" "/" link="${linked}" parents="${parents}" }
    onbuild { add "app/payload" "/added" unpack="${unpack}" }
    onbuild {
        run "cat /app/payload /added >/proof; echo cached >/cache/value; test \"$(/bin/busybox stat -c %%a /workspace)\" = 700 || exit 1; /bin/busybox grep ' /cache ' /proc/mounts | /bin/busybox grep nosuid | /bin/busybox grep nodev | /bin/busybox grep noexec" network="${network}" {
            mount "cache" target="/cache" nosuid=#true nodev=#true noexec=#true relabel="private"
            mount "tmpfs" target="/workspace" mode="0700" size="1m"
        }
    }
}
from "parent" {}
`, base.reference))
	layout := filepath.Join(f.root, "inherited-native-flags")
	var logs strings.Builder
	_, err := BuildDefinitionSupervised(f.ctx, def, planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{
		Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun",
		Output:              Output{Path: layout, Format: outputFormatDocker},
		SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath, Stdout: io.Discard, Stderr: &logs,
	})
	if err != nil {
		t.Fatalf("build: %v\n%s", err, logs.String())
	}
	raw, err := oci.ReadImageConfigLayout(f.ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	config, err := imageconfig.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	health, err := config.Healthcheck()
	if err != nil || health == nil || health.Interval != 1 {
		t.Fatalf("health=%#v error=%v", health, err)
	}
	if got := localComponentLastFile(t, layout, "proof"); got != "hello\nhello\n" {
		t.Fatalf("proof = %q", got)
	}
}

func TestOnBuildDynamicMountKindsExecuteInChildScope(t *testing.T) {
	f := newLocalComponentFixture(t)
	base := newLiveBusyBoxStorage(t, f.ctx, f.root, f.options.Store)
	f.write(t, "payload", "child-scope\n")
	def := parseWorkerDefinition(t, fmt.Sprintf(`from %q as="parent" {
    arg "bindkind" "secret"
    arg "cachekind" "secret"
    arg "tmpfskind" "secret"
    onbuild { arg "bindkind" "bind" }
    onbuild { arg "cachekind" "cache" }
    onbuild { arg "tmpfskind" "tmpfs" }
    onbuild {
        run "cat /source/payload >/proof; test \"$(/bin/busybox stat -c %%a /cache)\" = 700 || exit 1; echo cached >/cache/value; if touch /workspace/should-fail; then exit 1; fi" {
            mount "${bindkind}" source="." target="/source" readonly=#true
            mount "${cachekind}" target="/cache" mode="0700"
            mount "${tmpfskind}" target="/workspace" readonly=#true
        }
    }
}
from "parent" {}
`, base.reference))
	layout := filepath.Join(f.root, "inherited-dynamic-mounts")
	var logs strings.Builder
	_, err := BuildDefinitionSupervised(f.ctx, def, planner.Options{Mode: planner.Build, Platform: "linux/" + runtime.GOARCH}, SupervisedPlanOptions{
		Store: f.options.Store, ContextDir: f.contextDir, Isolation: "rootless", Runtime: "crun",
		Output:              Output{Path: layout, Format: outputFormatDocker},
		SignaturePolicyPath: f.options.SystemContext.SignaturePolicyPath, Stdout: io.Discard, Stderr: &logs,
	})
	if err != nil {
		t.Fatalf("build: %v\n%s", err, logs.String())
	}
	if got := localComponentLastFile(t, layout, "proof"); got != "child-scope\n" {
		t.Fatalf("proof=%q", got)
	}
}
