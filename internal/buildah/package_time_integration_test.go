package buildah

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPublicationTimestampAndTTLAcrossFreshBuildStores(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH for native package cache controls")
	}
	ctx := context.Background()
	root := t.TempDir()
	contextDir := filepath.Join(root, "context")
	if err := os.Mkdir(contextDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "payload"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(10, 0)
	if err := os.Chtimes(filepath.Join(contextDir, "payload"), old, old); err != nil {
		t.Fatal(err)
	}
	plan := testPublicationPlan(t, "package as=\"artifact\"\ncopy \"payload\" \"/payload\"\nextend\ncopy \"/payload\" \"/payload\" from=\"artifact\"\n")
	hour, expired := time.Hour, time.Nanosecond
	for _, test := range []struct {
		name   string
		epoch  int64
		ttl    time.Duration
		cached bool
	}{
		{"cold", 100, hour, false}, {"warm", 100, hour, true}, {"changed", 200, hour, false},
		{"expired", 200, expired, false}, {"refreshed", 200, hour, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := StoreOptions{GraphDriverName: "vfs", GraphRoot: filepath.Join(root, test.name, "graph"), RunRoot: filepath.Join(root, test.name, "run")}
			output := filepath.Join(root, test.name, "package.tar")
			options := PublicationOptions{PlanOptions: PlanOptions{Store: store, ContextDir: contextDir, Isolation: "rootless", CacheLocalDir: filepath.Join(root, "cache"), Timestamp: &test.epoch, CacheTTL: &test.ttl}, PackagePaths: map[string]string{"artifact": output}}
			packages, err := PublishPlan(ctx, plan, options)
			if err != nil {
				t.Fatal(err)
			}
			var config v1.Image
			if err := json.Unmarshal(packages["artifact"].Config, &config); err != nil {
				t.Fatal(err)
			}
			if config.Created == nil || config.Created.Unix() != test.epoch || len(config.History) == 0 || config.History[len(config.History)-1].Created == nil || config.History[len(config.History)-1].Created.Unix() != test.epoch {
				t.Fatalf("timestamped config=%+v", config)
			}
			file, err := os.Open(output)
			if err != nil {
				t.Fatal(err)
			}
			reader := tar.NewReader(file)
			found := false
			for {
				header, err := reader.Next()
				if err != nil {
					break
				}
				if header.Name == "payload" || header.Name == "./payload" {
					found = true
					if header.ModTime.Unix() != test.epoch {
						t.Fatalf("payload time=%v", header.ModTime)
					}
				}
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatal("missing payload")
			}
			lease, err := acquireStore(store)
			if err != nil {
				t.Fatal(err)
			}
			images, readErr := lease.store.Images()
			if err := errors.Join(readErr, lease.Close()); err != nil {
				t.Fatal(err)
			}
			if (len(images) == 0) != test.cached {
				t.Fatalf("cached=%v images=%d", test.cached, len(images))
			}
			options.Allow = []string{"bogus"}
			if _, err := PublishPlan(ctx, plan, options); err == nil {
				t.Fatal("warm package accepted unsupported entitlement")
			}
		})
	}
}
