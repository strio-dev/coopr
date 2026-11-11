package buildah

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/cache"
	"oras.land/oras-go/v2"
	orasoci "oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
)

// Exercise the actual supervised solver and stage-worker handoff over registry
// HTTP. Several ordinary cache sources expose alternative results for the same
// input keys. Neither restoration nor cache-to should transfer unused outputs;
// every selected output must still be complete and independently usable offline.
func TestInstructionGraphRegistryCacheSelectiveRestoreAndOfflineExport(t *testing.T) {
	requireLiveInstructionCache(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	root, input := copyDirectoryCacheContext(t)
	policy := writeComponentTestPolicy(t, root)
	plan := testPlan(t, `from "scratch" {
 copy "busybox" "/bin/busybox"
 run network="none" {
  exec "/bin/busybox" "sh" "-eu" "-c" "/bin/busybox od -An -N16 -tx1 /dev/urandom > /proof; /bin/busybox head -c 131072 /dev/urandom > /payload; echo REGISTRY_FRESH_RUN";
 }
}`)
	build := func(lane string, force bool, local string, from, to []CacheSpec) (Result, string, string) {
		t.Helper()
		layout := filepath.Join(root, lane, "layout")
		var stdout, stderr bytes.Buffer
		result, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: cacheTestStore(filepath.Join(root, lane)), ContextDir: input,
			Isolation: "rootless", Runtime: "crun", NoCache: force,
			CacheLocalDir: local, CacheFrom: from, CacheTo: to, TLSVerify: new(false),
			SignaturePolicyPath: policy, Output: Output{Path: layout}, Stdout: &stdout, Stderr: &stderr,
		})
		if err != nil {
			t.Fatalf("%s: %v\n%s\n%s", lane, err, stdout.String(), stderr.String())
		}
		if result.CacheStats.Errors != 0 {
			t.Fatalf("%s cache errors: %+v\n%s", lane, result.CacheStats, stderr.String())
		}
		return result, localComponentLastFile(t, layout, "proof"), stdout.String()
	}
	var latest string
	var sources []string
	catalog := instructionRegistryGraphCatalog{roots: map[digest.Digest][]string{}, results: map[string]cache.GraphResult{}, closures: map[string]map[digest.Digest]v1.Descriptor{}}
	for i := range 3 {
		source := filepath.Join(root, fmt.Sprintf("source-%d", i))
		_, proof, output := build(fmt.Sprintf("producer-%d", i), i != 0, source, nil, nil)
		if proof == latest || !strings.Contains(output, "REGISTRY_FRESH_RUN\n") {
			t.Fatal("producer failed to execute its fresh nondeterministic RUN", proof, output)
		}
		latest = proof
		sources = append(sources, source)
		generation := instructionRegistryCatalog(t, source)
		for key, ids := range generation.roots {
			catalog.roots[key] = append(catalog.roots[key], ids...)
		}
		maps.Copy(catalog.results, generation.results)
		maps.Copy(catalog.closures, generation.closures)
	}
	if len(catalog.results) < 4 {
		t.Fatalf("fixture did not retain three RUN alternatives: %d results", len(catalog.results))
	}

	traffic := &instructionRegistryTraffic{reads: map[digest.Digest]instructionRegistryRead{}, methods: map[string]int{}}
	var readOnly atomic.Bool
	handler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traffic.active.Add(1)
		defer traffic.active.Done()
		counted := &instructionRegistryResponse{ResponseWriter: w}
		if readOnly.Load() && r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(counted, "read-only test cache", http.StatusMethodNotAllowed)
		} else {
			handler.ServeHTTP(counted, r)
		}
		traffic.mu.Lock()
		defer traffic.mu.Unlock()
		traffic.methods[r.Method]++
		if strings.Contains(r.URL.Path, "/manifests/graph-") {
			traffic.graphTags++
		}
		if r.Method == http.MethodGet {
			traffic.getBytes += counted.bytes
			key := digest.Digest(filepath.Base(r.URL.Path))
			if key.Validate() == nil {
				value := traffic.reads[key]
				value.gets++
				value.bytes += counted.bytes
				traffic.reads[key] = value
			}
		}
	}))
	t.Cleanup(server.Close)
	var from []CacheSpec
	for index, source := range sources {
		repository := fmt.Sprintf("%s/coopr/native-selective-%d", strings.TrimPrefix(server.URL, "http://"), index)
		target, err := remote.NewRepository(repository)
		if err != nil {
			t.Fatal(err)
		}
		target.PlainHTTP = true
		layout, err := orasoci.New(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := layout.Tags(ctx, "", func(tags []string) error {
			for _, tag := range tags {
				if _, err := oras.Copy(ctx, layout, tag, target, tag, oras.DefaultCopyOptions); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		from = append(from, CacheSpec{Transport: "registry", Reference: repository})
	}
	readOnly.Store(true)
	traffic.reset()
	warm, proof, output := build("read-only", false, "", from, nil)
	if warm.CacheStats.Hits != 2 || warm.CacheStats.Misses != 0 || proof != latest || strings.Contains(output, "REGISTRY_FRESH_RUN\n") {
		t.Fatalf("read-only registry restore replayed or changed the selected result: proof=%q want=%q stats=%+v stdout=%s", proof, latest, warm.CacheStats, output)
	}
	selected := instructionRegistrySelectedResults(t, cacheTestStore(filepath.Join(root, "read-only")), catalog)
	if len(selected) != 2 {
		t.Fatalf("read-only registry restore imported %d native graph snapshots, want2", len(selected))
	}
	reads, methods, selectedBytes, graphTags := traffic.snapshot()
	if graphTags == 0 {
		t.Fatal("native consumer never requested a portable graph tag")
	}
	for method, count := range methods {
		if count != 0 && method != http.MethodGet && method != http.MethodHead {
			t.Fatalf("read-only consumer attempted registry %s", method)
		}
	}
	selectedBlobs := map[digest.Digest]v1.Descriptor{}
	for id := range selected {
		maps.Copy(selectedBlobs, catalog.closures[id])
	}
	unused := map[digest.Digest]v1.Descriptor{}
	for id, closure := range catalog.closures {
		if selected[id] {
			continue
		}
		for key, descriptor := range closure {
			if _, used := selectedBlobs[key]; !used {
				unused[key] = descriptor
			}
		}
	}
	var unusedPayloadBytes int64
	for key, descriptor := range unused {
		if reads[key].gets != 0 || reads[key].bytes != 0 {
			t.Errorf("read-only restore fetched unused %s: GETs=%d bytes=%d", key, reads[key].gets, reads[key].bytes)
		}
		if descriptor.MediaType != v1.MediaTypeImageManifest && descriptor.MediaType != "application/vnd.docker.distribution.manifest.v2+json" {
			unusedPayloadBytes += descriptor.Size
		}
	}
	if unusedPayloadBytes < 2*131072 {
		t.Fatalf("fixture lacks meaningful unused payloads: %d bytes", unusedPayloadBytes)
	}

	traffic.reset()
	mirror := filepath.Join(root, "selected")
	complete, proof, output := build("complete-consumer", false, "", from, []CacheSpec{{Transport: "oci-layout", Reference: mirror}})
	if complete.CacheStats.Hits != 2 || complete.CacheStats.Misses != 0 || proof != latest || strings.Contains(output, "REGISTRY_FRESH_RUN\n") {
		t.Fatalf("complete export replayed or changed the selected result: proof=%q want=%q stats=%+v stdout=%s", proof, latest, complete.CacheStats, output)
	}
	if got := instructionRegistrySelectedResults(t, cacheTestStore(filepath.Join(root, "complete-consumer")), catalog); len(got) != 2 {
		t.Fatalf("complete export imported %d native graph snapshots, want2", len(got))
	}
	completeReads, completeMethods, completeBytes, _ := traffic.snapshot()
	for key := range unused {
		if completeReads[key].gets != 0 || completeReads[key].bytes != 0 {
			t.Errorf("cache-to fetched unused HTTP content %s: %+v", key, completeReads[key])
		}
	}
	for method, count := range completeMethods {
		if count != 0 && method != http.MethodGet && method != http.MethodHead {
			t.Fatalf("local complete export attempted source registry %s", method)
		}
	}
	after := instructionGraphArtifactResults(t, mirror)
	retained := map[string]bool{}
	for key, ids := range after {
		if _, known := catalog.roots[key]; !known {
			t.Fatalf("selected export invented graph root %s", key)
		}
		for _, id := range ids {
			if !selected[id] {
				t.Errorf("cache-to mirrored unselected result %s in graph %s", id, key)
			}
			retained[id] = true
		}
	}
	if len(retained) != len(selected) {
		t.Fatalf("selected export lost required results: got %v, want %v", retained, selected)
	}
	// All origins are now unavailable. Complete local lookup verifies every
	// advertised payload, then a fresh store recovers the exact tested result.
	server.Close()
	local, err := cache.NewLocalStore(ctx, mirror, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for key := range after {
		record, restored, err := local.LookupGraph(ctx, key)
		if err != nil || record == nil || restored == "" {
			t.Fatal("complete graph cannot be restored offline", key, err)
		}
		if err := os.RemoveAll(restored); err != nil {
			t.Fatal(err)
		}
	}
	offline, proof, output := build("offline-consumer", false, "", []CacheSpec{{Transport: "oci-layout", Reference: mirror}}, nil)
	if offline.CacheStats.Hits != 2 || offline.CacheStats.Misses != 0 || proof != latest || strings.Contains(output, "REGISTRY_FRESH_RUN\n") {
		t.Fatalf("offline mirror replayed or changed the selected result: proof=%q want=%q stats=%+v stdout=%s", proof, latest, offline.CacheStats, output)
	}
	t.Logf("native registry restore: selected2snapshots, %d total GET-body bytes, %d unique unused payload bytes omitted; cache-to %d GET-body bytes without unused alternatives; exact offline result retained", selectedBytes, unusedPayloadBytes, completeBytes)
}

type instructionRegistryRead struct {
	gets  int
	bytes int64
}

type instructionRegistryTraffic struct {
	active    sync.WaitGroup
	mu        sync.Mutex
	reads     map[digest.Digest]instructionRegistryRead
	methods   map[string]int
	getBytes  int64
	graphTags int
}

func (traffic *instructionRegistryTraffic) reset() {
	traffic.active.Wait()
	traffic.mu.Lock()
	defer traffic.mu.Unlock()
	clear(traffic.reads)
	clear(traffic.methods)
	traffic.getBytes, traffic.graphTags = 0, 0
}

func (traffic *instructionRegistryTraffic) snapshot() (map[digest.Digest]instructionRegistryRead, map[string]int, int64, int) {
	traffic.active.Wait()
	traffic.mu.Lock()
	defer traffic.mu.Unlock()
	return maps.Clone(traffic.reads), maps.Clone(traffic.methods), traffic.getBytes, traffic.graphTags
}

type instructionRegistryResponse struct {
	http.ResponseWriter
	bytes int64
}

func (response *instructionRegistryResponse) Write(data []byte) (int, error) {
	n, err := response.ResponseWriter.Write(data)
	response.bytes += int64(n)
	return n, err
}

type instructionRegistryGraphCatalog struct {
	roots    map[digest.Digest][]string
	results  map[string]cache.GraphResult
	closures map[string]map[digest.Digest]v1.Descriptor
}

func instructionRegistryCatalog(t *testing.T, path string) instructionRegistryGraphCatalog {
	t.Helper()
	read := func(descriptor v1.Descriptor, value any) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(path, "blobs", descriptor.Digest.Algorithm().String(), descriptor.Digest.Encoded()))
		if err != nil || int64(len(data)) != descriptor.Size || descriptor.Digest.Algorithm().FromBytes(data) != descriptor.Digest {
			t.Fatalf("invalid fixture content %s: %v", descriptor.Digest, err)
		}
		if value != nil {
			if err := json.Unmarshal(data, value); err != nil {
				t.Fatal(err)
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(path, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index v1.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	catalog := instructionRegistryGraphCatalog{roots: instructionGraphArtifactResults(t, path), results: map[string]cache.GraphResult{}, closures: map[string]map[digest.Digest]v1.Descriptor{}}
	for _, descriptor := range index.Manifests {
		if descriptor.MediaType != v1.MediaTypeImageIndex {
			continue
		}
		var envelope v1.Index
		read(descriptor, &envelope)
		if envelope.ArtifactType != cache.GraphArtifactType || len(envelope.Manifests) == 0 {
			continue
		}
		var manifest v1.Manifest
		read(envelope.Manifests[0], &manifest)
		var record cache.GraphRecord
		read(manifest.Config, &record)
		if descriptor.Annotations[v1.AnnotationRefName] != "graph-v2-"+record.Root.Encoded() {
			continue
		}
		for _, result := range record.Results {
			catalog.results[result.ID] = result
		}
	}
	for id, result := range catalog.results {
		closure := map[digest.Digest]v1.Descriptor{}
		images := []v1.Descriptor{result.Image}
		if result.BaseImage != nil {
			images = append(images, *result.BaseImage)
		}
		for _, image := range images {
			var manifest v1.Manifest
			read(image, &manifest)
			closure[image.Digest] = image
			for _, payload := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
				read(payload, nil)
				closure[payload.Digest] = payload
			}
		}
		catalog.closures[id] = closure
	}
	return catalog
}

func instructionRegistrySelectedResults(t *testing.T, options StoreOptions, catalog instructionRegistryGraphCatalog) map[string]bool {
	t.Helper()
	selected := instructionGraphStoredResultIDs(t, options)
	for id := range selected {
		if _, exists := catalog.results[id]; !exists {
			t.Fatal("consumer retained an unknown graph result", id)
		}
	}
	return selected
}
