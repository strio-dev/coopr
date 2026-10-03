package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"coopr/internal/cache"
	"coopr/internal/planner"
)

func TestPackageCacheTTLRejectsStaleRecordsBeforeSeeding(t *testing.T) {
	ttl := time.Hour
	key := preflightPackageKey("bundle")
	for _, test := range []struct {
		name    string
		created time.Time
		hit     bool
	}{
		{"stale", time.Now().Add(-2 * time.Hour), false},
		{"fresh", time.Now(), true},
		{"unknown", time.Time{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "snapshot")
			if err := os.WriteFile(path, []byte("snapshot"), 0600); err != nil {
				t.Fatal(err)
			}
			seeded := false
			c := &packageResultCache{cacheTTL: &ttl,
				readStores: []cache.PackageStore{fakePackageStore{lookup: func(cache.PackageKey) (*cache.PackageRecord, string, error) {
					return &cache.PackageRecord{CreatedAt: test.created}, path, nil
				}}},
				writeStores: []cache.PackageStore{fakePackageStore{put: func(cache.PackageKey, cache.PackageRecord, string) error { seeded = true; return nil }}},
			}
			_, hit, err := c.lookupAll(context.Background(), map[string]cache.PackageKey{"stage": key})
			if err != nil || hit != test.hit || seeded != test.hit {
				t.Fatalf("hit=%v seeded=%v err=%v", hit, seeded, err)
			}
			if !hit {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("stale snapshot retained: %v", err)
				}
			}
		})
	}
}

func TestPackagePlanDigestIncludesForceTimestamp(t *testing.T) {
	plan := &planner.Plan{DefinitionType: "component", Platform: "linux/amd64"}
	baseline, err := packagePlanDigest(plan, nil, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	epoch := int64(123)
	seen := map[string]bool{baseline.String(): true}
	for _, options := range []PlanOptions{{Timestamp: &epoch}, {SourceDateEpoch: &epoch}, {SourceDateEpoch: &epoch, RewriteTimestamp: true}} {
		key, err := packagePlanDigest(plan, nil, options)
		if err != nil || seen[key.String()] {
			t.Fatalf("policy collided: %s %v", key, err)
		}
		seen[key.String()] = true
	}
}

func TestPackageTarForceTimestampRewritesOlderAndNewerFiles(t *testing.T) {
	var source, output bytes.Buffer
	writer := tar.NewWriter(&source)
	for _, epoch := range []int64{10, 200} {
		if err := writer.WriteHeader(&tar.Header{Name: time.Unix(epoch, 0).String(), Mode: 0600, Typeflag: tar.TypeReg, ModTime: time.Unix(epoch, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	forced := time.Unix(100, 0)
	if _, err := writePackageTar(context.Background(), &output, &source, nil, nil, &forced); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(&output)
	for range 2 {
		header, err := reader.Next()
		if err != nil || !header.ModTime.Equal(forced) {
			t.Fatalf("header=%+v err=%v", header, err)
		}
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatal(err)
	}
}
