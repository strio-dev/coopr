package apibackend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/strio-dev/coopr/api"
	"github.com/strio-dev/coopr/client"
)

func TestPublicClientNativeSelectedFileExports(t *testing.T) {
	f := newNativeAPIFixture(t)
	c, _ := publicNativeClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	request := func(label, command string) api.Request {
		r := f.request(label, "", "shared")
		r.Options.Output.Filesystem = ""
		r.Options.Network, r.Options.Isolation, r.Options.Runtime = "none", "rootless", "crun"
		r.Program.States[0].Operations = []api.Operation{
			{Kind: "copy", Sources: []string{"busybox"}, Destination: "/bin/busybox"},
			{Kind: "run", Args: []string{"/bin/busybox", "sh", "-eu", "-c", command}},
		}
		return r
	}
	const command = "/bin/busybox mkdir /reports; /bin/busybox cp /bin/busybox /reports/tool; /bin/busybox od -An -N16 -tx1 /dev/urandom > /reports/proof; printf 'artifact\\n' > /artifact"
	coldRequest := request("selected-cold", command)
	coldRequest.Options.Output.Exports = []api.FileExport{{Source: "/reports", Destination: filepath.Join(f.root, "cold-reports")}, {Source: "/artifact", Destination: filepath.Join(f.root, "cold-artifact")}}
	cold, err := c.Execute(ctx, coldRequest, client.StreamOptions{})
	if err != nil || len(cold.Exports) != 2 || cold.Exports[0].Status != "passed" || cold.Exports[1].Status != "passed" || cold.Filesystem != "" {
		t.Fatalf("selected cold export: %+v %v", cold, err)
	}
	proof := publicNativeRead(t, filepath.Join(cold.Exports[0].Destination, "proof"))
	if publicNativeRead(t, cold.Exports[1].Destination) != "artifact\n" {
		t.Fatal("selected file bytes changed")
	}
	info, err := os.Stat(filepath.Join(cold.Exports[0].Destination, "tool"))
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("exported executable mode: %v %v", info, err)
	}
	warmRequest := request("selected-warm", command)
	warmRequest.Options.Output.Exports = []api.FileExport{{Source: "/reports/proof", Destination: filepath.Join(f.root, "warm-proof")}}
	warm, err := c.Execute(ctx, warmRequest, client.StreamOptions{})
	if err != nil || len(warm.Exports) != 1 || warm.Exports[0].Status != "passed" || warm.Cache == nil || warm.Cache.Hits != 1 || warm.ImageID != cold.ImageID || warm.ManifestDigest != cold.ManifestDigest || publicNativeRead(t, warm.Exports[0].Destination) != proof {
		t.Fatalf("export selection salted image identity or reran RUN: cold=%+v warm=%+v err=%v", cold, warm, err)
	}

	for _, prerequisite := range []bool{false, true} {
		label := "selected-run-failed"
		if prerequisite {
			label = "selected-prerequisite-failed"
		}
		r := request(label, "printf 'failed diagnostic\\n' > /report; exit 23")
		r.Options.Output.Exports = []api.FileExport{{Source: "/report", Destination: filepath.Join(f.root, label+".txt")}}
		if prerequisite {
			r.Program.States = append(r.Program.States, api.State{ID: "dependent", From: api.Source{State: "result"}})
			r.Program.Output = "dependent"
		}
		result, err := c.Execute(ctx, r, client.StreamOptions{})
		var failure *api.Error
		if !errors.As(err, &failure) || failure.Code != "execution_failed" || !strings.Contains(err.Error(), "23") || len(result.Exports) != 1 {
			t.Fatalf("failed RUN became successful or lost receipt: %+v %v", result, err)
		}
		if prerequisite {
			if result.Exports[0].Status != "skipped" {
				t.Fatalf("failed prerequisite exported discarded state: %+v", result.Exports)
			}
			if _, err := os.Lstat(r.Options.Output.Exports[0].Destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed prerequisite published a file: %v", err)
			}
		} else if result.Exports[0].Status != "passed" || publicNativeRead(t, result.Exports[0].Destination) != "failed diagnostic\n" {
			t.Fatalf("ordinary selected RUN failure lost report: %+v", result.Exports)
		}
		if _, err := os.Lstat(r.Options.Output.OCI); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed execution published an image: %v", err)
		}
	}
}

func TestPublicClientNativeSelectedFileExportsDefinition(t *testing.T) {
	f := newNativeAPIFixture(t)
	c, _ := publicNativeClient(t)
	definition := filepath.Join(f.context, "export.coopr")
	publicNativeWrite(t, definition, `from "scratch" {
 copy "busybox" "/bin/busybox"
 run network="none" { exec "/bin/busybox" "sh" "-c" "printf definition > /report; exit 23"; }
}
`, 0600)
	r := publicNativeDefinition(f, definition, "build", "definition-selected", filepath.Join(f.root, "components"))
	r.Options.Network, r.Options.Isolation, r.Options.Runtime = "none", "rootless", "crun"
	r.Options.Output.Filesystem = ""
	r.Options.Output.Exports = []api.FileExport{{Source: "/report", Destination: filepath.Join(f.root, "definition-report")}}
	result, err := c.Execute(t.Context(), r, client.StreamOptions{})
	if err == nil || len(result.Exports) != 1 || result.Exports[0].Status != "passed" || publicNativeRead(t, result.Exports[0].Destination) != "definition" {
		t.Fatalf("definition failure lost selected report: %+v %v", result, err)
	}
}
