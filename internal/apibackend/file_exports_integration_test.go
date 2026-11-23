package apibackend

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/strio-dev/coopr/api"
	"github.com/strio-dev/coopr/client"
	"github.com/strio-dev/coopr/internal/buildah"
)

func TestPublicClientNativeSelectedStateExports(t *testing.T) {
	f := newNativeAPIFixture(t)
	c, _ := publicNativeClient(t)
	request := func(label, store string) api.Request {
		r := f.request(label, "", store)
		r.Options.Output.OCI, r.Options.Output.Filesystem = "", ""
		r.Options.Network, r.Options.Isolation = "none", "rootless"
		r.Program = &api.Program{Output: "final", States: []api.State{
			{ID: "prefix", From: api.Source{Image: "scratch"}, Operations: []api.Operation{
				{Kind: "copy", Sources: []string{"busybox"}, Destination: "/bin/busybox"},
				{Kind: "run", Args: []string{"/bin/busybox", "sh", "-c", "printf v1 > /report; echo STOKE_EXPORT_PREFIX_EXECUTED"}},
			}},
			{ID: "middle", From: api.Source{State: "prefix"}, Operations: []api.Operation{{Kind: "run", Args: []string{"/bin/busybox", "sh", "-c", "printf v2 > /report; echo STOKE_EXPORT_LATER_EXECUTED"}}}},
			{ID: "final", From: api.Source{State: "middle"}, Operations: []api.Operation{{Kind: "run", Args: []string{"/bin/busybox", "sh", "-c", "printf final > /final; echo STOKE_EXPORT_FINAL_EXECUTED"}}}},
		}}
		r.Options.Output.Exports = []api.FileExport{{State: "prefix", Source: "/report", Destination: filepath.Join(f.root, label+"-prefix")}, {Source: "/report", Destination: filepath.Join(f.root, label+"-final")}}
		return r
	}
	var coldLog, warmLog bytes.Buffer
	coldRequest, warmRequest := request("state-cold", "shared"), request("state-warm", "fresh")
	cold, err := c.Execute(t.Context(), coldRequest, client.StreamOptions{Stdout: &coldLog, Stderr: &coldLog})
	if err != nil {
		t.Fatalf("cold state exports: %v\n%s", err, coldLog.String())
	}
	warm, err := c.Execute(t.Context(), warmRequest, client.StreamOptions{Stdout: &warmLog, Stderr: &warmLog})
	if err != nil || cold.OCI != "" || warm.OCI != "" || cold.ManifestDigest != "" || warm.ManifestDigest != "" || cold.ImageID == "" || warm.ImageID != cold.ImageID || warm.Cache == nil || warm.Cache.Hits != 2 || warm.Cache.Misses != 0 {
		t.Fatalf("warm state exports: cold=%+v warm=%+v err=%v\n%s", cold, warm, err, warmLog.String())
	}
	for _, marker := range []string{"STOKE_EXPORT_PREFIX_EXECUTED", "STOKE_EXPORT_LATER_EXECUTED", "STOKE_EXPORT_FINAL_EXECUTED"} {
		if !slices.Contains(strings.Split(coldLog.String(), "\n"), marker) || slices.Contains(strings.Split(warmLog.String(), "\n"), marker) {
			t.Fatalf("warm stage reran or cold RUN was absent: %s\ncold:%s\nwarm:%s", marker, coldLog.String(), warmLog.String())
		}
	}
	for _, result := range []api.Result{cold, warm} {
		if len(result.Exports) != 2 || result.Exports[0].State != "prefix" || result.Exports[1].State != "" {
			t.Fatalf("public selector/order changed: %+v", result.Exports)
		}
		for i, want := range []string{"v1", "v2"} {
			if result.Exports[i].Status != "passed" || publicNativeRead(t, result.Exports[i].Destination) != want {
				t.Fatalf("export did not capture its boundary: %+v", result.Exports)
			}
		}
	}
	usage, err := buildah.MaintainStoreSupervised(t.Context(), buildah.StoreMaintenanceRequest{Mode: buildah.StoreMaintenanceDF, Store: buildah.StoreOptions{RunRoot: warmRequest.Options.Store.RunRoot, GraphRoot: warmRequest.Options.Store.GraphRoot, GraphDriverName: "vfs"}})
	if err != nil || usage.Images != 2 {
		t.Fatalf("warm exports materialized unrequested ancestor images: images=%d err=%v", usage.Images, err)
	}
}

func TestPublicClientNativeSelectedStateExportsRetainEarlierFailureReports(t *testing.T) {
	f := newNativeAPIFixture(t)
	c, _ := publicNativeClient(t)
	r := f.request("state-failed", "", "shared")
	r.Options.Output.Filesystem = ""
	r.Options.Network, r.Options.Isolation = "none", "rootless"
	r.Program = &api.Program{Output: "finish", States: []api.State{
		{ID: "prepare", From: api.Source{Image: "scratch"}, Operations: []api.Operation{{Kind: "copy", Sources: []string{"busybox"}, Destination: "/bin/busybox"}, {Kind: "run", Args: []string{"/bin/busybox", "sh", "-c", "printf v1 > /report"}}}},
		{ID: "0", From: api.Source{State: "prepare"}, Operations: []api.Operation{{Kind: "run", Args: []string{"/bin/busybox", "sh", "-c", "printf v2 > /report"}}}},
		{ID: "failed", From: api.Source{State: "0"}, Operations: []api.Operation{{Kind: "run", Args: []string{"/bin/busybox", "sh", "-c", "printf diagnostic > /report; exit 23"}}}},
		{ID: "finish", From: api.Source{State: "failed"}},
	}}
	for _, state := range []string{"0", "prepare", "failed", "finish"} {
		r.Options.Output.Exports = append(r.Options.Output.Exports, api.FileExport{State: state, Source: "/report", Destination: filepath.Join(f.root, "state-receipt-"+state)})
	}
	result, err := c.Execute(t.Context(), r, client.StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), "23") || len(result.Exports) != 4 {
		t.Fatalf("failed state lost original error/receipts: %+v %v", result, err)
	}
	for i, want := range []string{"v2", "v1", "diagnostic"} {
		if result.Exports[i].State != r.Options.Output.Exports[i].State || result.Exports[i].Status != "passed" || publicNativeRead(t, result.Exports[i].Destination) != want {
			t.Fatalf("earlier capture lost or request order changed: %+v", result.Exports)
		}
	}
	if result.Exports[3].State != "finish" || result.Exports[3].Status != "skipped" {
		t.Fatalf("failed prerequisite exported a nonexistent later state: %+v", result.Exports)
	}
	for _, output := range []string{r.Options.Output.OCI, r.Options.Output.Exports[3].Destination} {
		if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed state opened success output: %s: %v", output, err)
		}
	}
}

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
	warmRequest.Options.Output.Exports = []api.FileExport{{Source: "/reports/proof", Destination: filepath.Join(f.root, "warm-proof")}, {Source: "/reports", Destination: filepath.Join(f.root, "warm-reports")}}
	warm, err := c.Execute(ctx, warmRequest, client.StreamOptions{})
	if err != nil || len(warm.Exports) != 2 || warm.Exports[0].Status != "passed" || warm.Exports[1].Status != "passed" || warm.Cache == nil || warm.Cache.Hits != 1 || warm.ImageID != cold.ImageID || warm.ManifestDigest != cold.ManifestDigest || publicNativeRead(t, warm.Exports[0].Destination) != proof {
		t.Fatalf("export selection salted image identity or reran RUN: cold=%+v warm=%+v err=%v", cold, warm, err)
	}
	if publicNativeRead(t, filepath.Join(warm.Exports[1].Destination, "proof")) != proof {
		t.Fatal("cached directory export changed its proof")
	}
	coldTool, coldErr := os.ReadFile(filepath.Join(cold.Exports[0].Destination, "tool"))
	warmTool, warmErr := os.ReadFile(filepath.Join(warm.Exports[1].Destination, "tool"))
	if err := errors.Join(coldErr, warmErr); err != nil || !bytes.Equal(coldTool, warmTool) {
		t.Fatalf("cached directory export changed its executable bytes: %v", err)
	}
	info, err = os.Stat(filepath.Join(warm.Exports[1].Destination, "tool"))
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("cached directory executable mode: %v %v", info, err)
	}

	gate := request("native-only-gate", command+"; echo NATIVE_ONLY_GATE_RAN")
	unrequestedOCI := gate.Options.Output.OCI
	gate.Options.Output.OCI = ""
	gate.Options.Cache = api.Cache{NoCache: true, MountDir: filepath.Join(f.root, "native-only-mounts")}
	var gateLog bytes.Buffer
	imageExports := 0
	gateResult, err := c.Execute(ctx, gate, client.StreamOptions{Stdout: &gateLog, Progress: func(event api.OperationProgress) {
		if event.Phase == "export" && event.Instruction == "image" {
			imageExports++
		}
	}})
	if err != nil || gateResult.ImageID == "" || gateResult.OCI != "" || gateResult.ManifestDigest != "" || !slices.Contains(strings.Split(gateLog.String(), "\n"), "NATIVE_ONLY_GATE_RAN") || imageExports != 0 {
		t.Fatalf("native-only gate serialized an image or did not execute: %+v exports=%d err=%v\n%s", gateResult, imageExports, err, gateLog.String())
	}
	if gateResult.Cache == nil || gateResult.Cache.Stored != 0 {
		t.Fatalf("native-only gate created an undeclared cache writer: %+v", gateResult.Cache)
	}
	if _, err := os.Lstat(unrequestedOCI); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native-only gate created an OCI output: %v", err)
	}

	for _, prerequisite := range []bool{false, true} {
		label := "selected-run-failed"
		if prerequisite {
			label = "selected-prerequisite-failed"
		}
		r := request(label, "printf 'failed diagnostic\\n' > /report; exit 23")
		unrequestedOCI := r.Options.Output.OCI
		r.Options.Output.OCI = ""
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
		if _, err := os.Lstat(unrequestedOCI); !errors.Is(err, os.ErrNotExist) {
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

func TestSelectedFileExportsRetainReceiptsAfterOtherOutputFailure(t *testing.T) {
	f := newNativeAPIFixture(t)
	r := f.request("other-output-failure", "", "shared")
	r.Program.States[0].Operations = []api.Operation{{Kind: "copy", Sources: []string{"busybox"}, Destination: "/tool"}}
	r.Options.Output.Exports = []api.FileExport{{Source: "/tool", Destination: filepath.Join(f.root, "selected-tool")}}
	var collision error
	ctx := api.WithProgressObserver(t.Context(), func(event api.OperationProgress) {
		if event.Instruction == "cache" && event.Phase == "export" && event.State == "completed" {
			collision = os.Mkdir(r.Options.Output.Filesystem, 0700)
		}
	})
	result, err := Execute(ctx, r, io.Discard, io.Discard)
	if collision != nil || err == nil || !strings.Contains(err.Error(), "promote filesystem output") || len(result.Exports) != 1 || result.Exports[0].Status != "passed" {
		t.Fatalf("other output failure lost already-published file: result=%+v err=%v collision=%v", result, err, collision)
	}
	if info, err := os.Stat(r.Options.Output.Exports[0].Destination); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("reported completed export missing: %v %v", info, err)
	}
}
