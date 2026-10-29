// This independent frontend imports only Coopr's public execution package.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/strio-dev/coopr"
)

func main() {
	if coopr.InitReexec() {
		return
	}
	busybox := flag.String("busybox", "", "absolute path to a static BusyBox binary")
	work := flag.String("work", "", "fresh absolute directory for caller-owned stores, cache and outputs")
	flag.Parse()
	if err := prove(*work, *busybox); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type proof struct {
	Cold            coopr.CacheStats `json:"cold"`
	Warm            coopr.CacheStats `json:"warm"`
	Overlap         bool             `json:"independent_branches_overlapped"`
	Workers         int              `json:"cancelled_processes_observed"`
	Clean           bool             `json:"cleanup_verified"`
	ContextIsolated bool             `json:"writable_context_isolated"`
}

func prove(root, busybox string) error {
	if os.Getuid() == 0 {
		return errors.New("run this consumer as an ordinary user with subordinate IDs")
	}
	if !filepath.IsAbs(root) || !filepath.IsAbs(busybox) {
		return errors.New("-work and -busybox must be absolute paths")
	}
	if err := os.Mkdir(root, 0700); err != nil {
		return err
	}
	for _, dir := range []string{"context", "tmp"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			return err
		}
	}
	// Isolate disposable engine files so cleanup is observable without inspecting
	// unrelated host activity. These are ordinary caller-owned process settings.
	if err := os.Setenv("TMPDIR", filepath.Join(root, "tmp")); err != nil {
		return err
	}
	if err := copyBinary(busybox, filepath.Join(root, "context", "busybox")); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "context", "caller-file"), []byte("original\n"), 0600); err != nil {
		return err
	}
	policy := filepath.Join(root, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`), 0600); err != nil {
		return err
	}
	options := func(name string) coopr.Options {
		return coopr.Options{
			Store:      coopr.StoreOptions{RunRoot: filepath.Join(root, name+"-run"), GraphRoot: filepath.Join(root, name+"-graph"), Driver: "vfs"},
			ContextDir: filepath.Join(root, "context"), Runtime: "crun", Isolation: "rootless", Jobs: 2,
			Output:   coopr.Output{OCI: filepath.Join(root, name+"-oci"), Filesystem: filepath.Join(root, name+"-fs")},
			Cache:    coopr.CacheOptions{LocalDir: filepath.Join(root, "cache")},
			Registry: coopr.RegistryOptions{SignaturePolicyPath: policy},
			Stdout:   os.Stderr, Stderr: os.Stderr,
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := inContextExport(ctx, root, options("export-cold"), options("export-warm")); err != nil {
		return err
	}
	coldOptions := options("cold")
	cold, err := coopr.Execute(ctx, graph(), coldOptions)
	if err != nil {
		return fmt.Errorf("cold graph: %w", err)
	}
	if err := exported(cold); err != nil {
		return err
	}
	if cold.Cache.Stored == 0 || cold.Cache.Errors != 0 {
		return fmt.Errorf("cold portable cache was not stored cleanly: %+v", cold.Cache)
	}
	if err := clean(root, coldOptions); err != nil {
		return err
	}
	// A fresh native store prevents Buildah's local layer cache from satisfying
	// this proof without consulting the portable cache measured by CacheStats.
	warmOptions := options("warm")
	warm, err := coopr.Execute(ctx, graph(), warmOptions)
	if err != nil {
		return fmt.Errorf("warm fresh-store graph: %w", err)
	}
	if err := exported(warm); err != nil {
		return err
	}
	if warm.Cache.Hits == 0 || warm.Cache.Errors != 0 {
		return fmt.Errorf("warm portable cache did not hit cleanly: %+v", warm.Cache)
	}
	if err := clean(root, warmOptions); err != nil {
		return err
	}
	contextOptions := options("context")
	contextOptions.Cache = coopr.CacheOptions{NoCache: true}
	contextResult, err := coopr.Execute(ctx, contextGraph(), contextOptions)
	if err != nil {
		return fmt.Errorf("writable context graph: %w", err)
	}
	for path, want := range map[string]string{
		filepath.Join(contextResult.Filesystem, "context-write"): "isolated\n",
		filepath.Join(root, "context", "caller-file"):            "original\n",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(data) != want {
			return fmt.Errorf("context isolation at %s: got %q, want %q", path, data, want)
		}
	}
	if err := clean(root, contextOptions); err != nil {
		return err
	}
	cancelledOptions := options("cancelled")
	cancelledOptions.Cache = coopr.CacheOptions{NoCache: true}
	ready := &readiness{Writer: os.Stderr, ready: make(chan struct{})}
	cancelledOptions.Stdout = ready
	cancelledOptions.Stderr = ready
	cancelCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { _, err := coopr.Execute(cancelCtx, cancellationGraph(), cancelledOptions); done <- err }()
	select {
	case <-ready.ready:
	case err := <-done:
		return fmt.Errorf("cancellation graph exited before readiness: %w", err)
	case <-ctx.Done():
		stop()
		<-done
		return fmt.Errorf("waiting for cancellation readiness: %w", ctx.Err())
	}
	processes, err := descendants(os.Getpid())
	stop()
	runErr := <-done
	if err != nil {
		return err
	}
	if len(processes) == 0 {
		return errors.New("readiness observed without any worker descendants")
	}
	if !errors.Is(runErr, context.Canceled) {
		return fmt.Errorf("expected context cancellation, got %v", runErr)
	}
	if err := waitGone(ctx, processes); err != nil {
		return err
	}
	for _, path := range []string{cancelledOptions.Output.OCI, cancelledOptions.Output.Filesystem} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cancelled output retained at %s: %v", path, err)
		}
	}
	if err := clean(root, cancelledOptions); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(proof{Cold: cold.Cache, Warm: warm.Cache, Overlap: true, Workers: len(processes), Clean: true, ContextIsolated: true})
}

func inContextExport(ctx context.Context, root string, cold, warm coopr.Options) error {
	contextDir := filepath.Join(root, "export-context")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(contextDir, "source.txt"), []byte("context input\n"), 0600); err != nil {
		return err
	}
	cold.ContextDir = contextDir
	cold.Output.Filesystem = filepath.Join(contextDir, "export")
	cold.Cache.LocalDir = filepath.Join(root, "export-cache")
	warm.ContextDir, warm.Output, warm.Cache = cold.ContextDir, cold.Output, cold.Cache
	program := coopr.Program{States: []coopr.State{{
		ID: "copied", From: coopr.Source{Image: "scratch"},
		Operations: []coopr.Operation{coopr.Copy{Sources: []string{"."}, Destination: "/copied"}},
	}}, Output: "copied"}
	for index, options := range []coopr.Options{cold, warm} {
		result, err := coopr.Execute(ctx, program, options)
		if err != nil {
			return fmt.Errorf("in-context export %d: %w", index, err)
		}
		copied := filepath.Join(result.Filesystem, "copied")
		entries, err := os.ReadDir(copied)
		if err != nil {
			return err
		}
		if len(entries) != 1 || entries[0].Name() != "source.txt" {
			names := make([]string, len(entries))
			for i, entry := range entries {
				names[i] = entry.Name()
			}
			return fmt.Errorf("COPY '.' included non-input entries: %v", names)
		}
		data, err := os.ReadFile(filepath.Join(copied, "source.txt"))
		if err != nil {
			return err
		}
		if string(data) != "context input\n" {
			return fmt.Errorf("unexpected in-context COPY data %q", data)
		}
		if result.Cache.Errors != 0 || (index == 0 && result.Cache.Stored == 0) || (index == 1 && result.Cache.Hits == 0) {
			return fmt.Errorf("in-context export %d cache did not restore cleanly: %+v", index, result.Cache)
		}
		if err := clean(root, options); err != nil {
			return err
		}
		if index == 0 {
			// Successful outputs belong to the caller. Remove our cold outputs so
			// the next build receives exactly the same context, not an old export.
			for _, path := range []string{result.OCI, result.Filesystem} {
				if err := os.RemoveAll(path); err != nil {
					return err
				}
			}
			entries, err := os.ReadDir(contextDir)
			if err != nil {
				return err
			}
			if len(entries) != 1 || entries[0].Name() != "source.txt" {
				return errors.New("in-context export changed its caller-owned input context")
			}
		}
	}
	return nil
}

func base() coopr.State {
	return coopr.State{ID: "base", From: coopr.Source{Image: "scratch"}, Operations: []coopr.Operation{
		coopr.Copy{Sources: []string{"busybox"}, Destination: "/bin/busybox"},
	}}
}
func graph() coopr.Program {
	branch := func(name, other string) coopr.State {
		// Neither RUN can complete until the other RUN has started. This is a
		// rendezvous, not an elapsed-time performance assertion.
		script := fmt.Sprintf("set -eu; /bin/busybox touch /coord/%s; until test -f /coord/%s; do /bin/busybox sleep 0.02; done; printf '%s\\n' > /%s", name, other, name, name)
		return coopr.State{ID: coopr.StateID(name), From: coopr.Source{State: "base"}, Operations: []coopr.Operation{
			coopr.Run{Args: []string{"/bin/busybox", "sh", "-c", script}, Mounts: []coopr.Mount{{Type: "cache", Target: "/coord", ID: "library-rendezvous", Sharing: "shared"}}},
		}}
	}
	return coopr.Program{States: []coopr.State{base(), branch("left", "right"), branch("right", "left"), {
		ID: "joined", From: coopr.Source{State: "left"}, Operations: []coopr.Operation{
			coopr.Copy{Sources: []string{"/right"}, Destination: "/right", From: coopr.Source{State: "right"}},
			coopr.Run{Args: []string{"/bin/busybox", "sh", "-c", "/bin/busybox cat /left /right > /result"}},
		},
	}}, Output: "joined"}
}
func contextGraph() coopr.Program {
	return coopr.Program{States: []coopr.State{base(), {
		ID: "context-write", From: coopr.Source{State: "base"}, Operations: []coopr.Operation{
			coopr.Env{Name: "LITERAL", Value: "${literal}"},
			coopr.Run{Args: []string{"/bin/busybox", "sh", "-c", `set -eu; test "$LITERAL" = '${literal}'; printf 'isolated\n' > /context/caller-file`}, Mounts: []coopr.Mount{{Type: "bind", Target: "/context", ReadOnly: false}}},
			coopr.Copy{Sources: []string{"caller-file"}, Destination: "/context-write"},
		},
	}}, Output: "context-write"}
}
func cancellationGraph() coopr.Program {
	return coopr.Program{States: []coopr.State{base(), {ID: "wait", From: coopr.Source{State: "base"}, Operations: []coopr.Operation{
		coopr.Run{Args: []string{"/bin/busybox", "sh", "-c", "/bin/busybox sleep 300 & printf 'library-worker-ready\\n'; wait"}},
	}}}, Output: "wait"}
}
func exported(result coopr.Result) error {
	data, err := os.ReadFile(filepath.Join(result.Filesystem, "result"))
	if err != nil {
		return err
	}
	if string(data) != "left\nright\n" {
		return fmt.Errorf("unexpected dependent COPY output %q", data)
	}
	if result.ImageID == "" || result.ManifestDigest == "" {
		return errors.New("missing exported image identity")
	}
	_, err = os.Stat(filepath.Join(result.OCI, "index.json"))
	return err
}
func copyBinary(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	return errors.Join(copyErr, output.Close())
}
func clean(root string, options coopr.Options) error {
	// The on-disk list is containers/storage's public persistent JSON format;
	// inspect it without importing any Coopr internal or backend packages.
	path := filepath.Join(options.Store.GraphRoot, "vfs-containers", "containers.json")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		var containers []json.RawMessage
		if err := json.Unmarshal(data, &containers); err != nil {
			return err
		}
		if len(containers) != 0 {
			return fmt.Errorf("retained %d containers in %s", len(containers), path)
		}
	}
	for _, directory := range []string{filepath.Join(root, "tmp"), root} {
		entries, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if directory == filepath.Join(root, "tmp") || strings.HasPrefix(entry.Name(), ".coopr-") {
				return fmt.Errorf("disposable engine path retained: %s", filepath.Join(directory, entry.Name()))
			}
		}
	}
	return nil
}

type readiness struct {
	io.Writer
	mu    sync.Mutex
	text  string
	ready chan struct{}
	once  sync.Once
}

func (w *readiness) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.text += string(p)
	if strings.Contains(w.text, "library-worker-ready\n") {
		w.once.Do(func() { close(w.ready) })
	}
	return w.Writer.Write(p)
}

type process struct {
	pid   int
	start string
}
type procInfo struct {
	parent int
	start  string
}

func readProcess(pid int) (procInfo, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return procInfo{}, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return procInfo{}, errors.New("invalid proc stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return procInfo{}, errors.New("short proc stat")
	}
	parent, err := strconv.Atoi(fields[1])
	return procInfo{parent: parent, start: fields[19]}, err
}
func descendants(parent int) ([]process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	all := map[int]procInfo{}
	for _, entry := range entries {
		pid, e := strconv.Atoi(entry.Name())
		if e != nil {
			continue
		}
		if info, e := readProcess(pid); e == nil {
			all[pid] = info
		}
	}
	known := map[int]bool{parent: true}
	result := []process{}
	for changed := true; changed; {
		changed = false
		for pid, info := range all {
			if !known[pid] && known[info.parent] {
				known[pid] = true
				result = append(result, process{pid: pid, start: info.start})
				changed = true
			}
		}
	}
	return result, nil
}
func waitGone(ctx context.Context, processes []process) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		retained := 0
		for _, p := range processes {
			info, err := readProcess(p.pid)
			if err == nil && info.start == p.start {
				retained++
			}
		}
		if retained == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%d cancelled processes still running: %w", retained, ctx.Err())
		case <-ticker.C:
		}
	}
}
