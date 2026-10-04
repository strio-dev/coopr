package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
)

func TestPackagePlanDigestIncludesTimestampPolicy(t *testing.T) {
	epoch := int64(123)
	plan := &planner.Plan{DefinitionType: "component", Platform: "linux/amd64", SourceDateEpoch: &epoch}
	base, err := packagePlanDigest(plan, nil, PlanOptions{SourceDateEpoch: plan.SourceDateEpoch})
	if err != nil {
		t.Fatal(err)
	}
	rewritten, err := packagePlanDigest(plan, nil, PlanOptions{SourceDateEpoch: plan.SourceDateEpoch, RewriteTimestamp: true})
	if err != nil {
		t.Fatal(err)
	}
	if base == rewritten {
		t.Fatal("rewrite-timestamp did not change package plan digest")
	}
	epoch = 124
	changed, err := packagePlanDigest(plan, nil, PlanOptions{SourceDateEpoch: plan.SourceDateEpoch})
	if err != nil {
		t.Fatal(err)
	}
	if base == changed {
		t.Fatal("SOURCE_DATE_EPOCH did not change package plan digest")
	}
}

func TestStageWorkerMessagePreservesTimestampPolicy(t *testing.T) {
	epoch := int64(123)
	request := stageWorkerRequest{SourceDateEpoch: &epoch, RewriteTimestamp: true, ResultPath: t.TempDir() + "/result"}
	path := t.TempDir() + "/request"
	if err := writeWorkerJSON(path, request); err != nil {
		t.Fatal(err)
	}
	var decoded stageWorkerRequest
	if err := readWorkerJSON(path, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SourceDateEpoch == nil || *decoded.SourceDateEpoch != epoch || !decoded.RewriteTimestamp {
		t.Fatalf("timestamp policy = %#v, %v", decoded.SourceDateEpoch, decoded.RewriteTimestamp)
	}
}

func TestWritePackageTarClampsNewerTimesOnly(t *testing.T) {
	old := time.Unix(50, 0).UTC()
	newer := time.Unix(150, 0).UTC()
	maximum := time.Unix(100, 0).UTC()
	var source bytes.Buffer
	w := tar.NewWriter(&source)
	for _, entry := range []struct {
		name  string
		stamp time.Time
	}{{"old", old}, {"new", newer}} {
		if err := w.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o644, ModTime: entry.stamp, AccessTime: entry.stamp, ChangeTime: entry.stamp}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if _, err := writePackageTar(context.Background(), &output, &source, nil, &maximum, nil); err != nil {
		t.Fatal(err)
	}
	r := tar.NewReader(&output)
	for _, want := range []time.Time{old, maximum} {
		header, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !header.ModTime.Equal(want) {
			t.Fatalf("%s mtime = %v, want %v", header.Name, header.ModTime, want)
		}
	}
}

func TestBuildPlanSourceDateEpochMetadataAndCache(t *testing.T) {
	if os.Getenv("COOPR_TEST_BUILDAH") == "" {
		t.Skip("set COOPR_TEST_BUILDAH=1 for a live SOURCE_DATE_EPOCH build")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "two"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}
	build := func(epoch int64, name string) (string, any) {
		def, err := definition.Parse(strings.NewReader("from \"scratch\"\ncopy \"one\" \"/one\"\ncopy \"two\" \"/two\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		plan, err := planner.Create(def, planner.Options{Mode: planner.Build, Platform: "linux/amd64", Arguments: map[string]string{"SOURCE_DATE_EPOCH": fmt.Sprint(epoch)}})
		if err != nil {
			t.Fatal(err)
		}
		layout := filepath.Join(root, name)
		if _, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
			Store: store, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout}, Stdout: io.Discard, Stderr: io.Discard,
		}); err != nil {
			t.Fatal(err)
		}
		_, image := readPlanImage(t, layout)
		want := time.Unix(epoch, 0).UTC()
		if image.Created == nil || !image.Created.Equal(want) {
			t.Fatalf("created = %v, want %v", image.Created, want)
		}
		for index, history := range image.History {
			if history.Created == nil || !history.Created.Equal(want) {
				t.Fatalf("history %d created = %v, want %v", index, history.Created, want)
			}
		}
		descriptor, err := oci.LayoutRoot(layout)
		if err != nil {
			t.Fatal(err)
		}
		if descriptor.Annotations["org.opencontainers.image.created"] != want.Format(time.RFC3339) {
			t.Fatalf("created annotation = %q", descriptor.Annotations["org.opencontainers.image.created"])
		}
		return descriptor.Digest.String(), image
	}
	firstDigest, _ := build(123, "epoch-a")
	secondDigest, secondImage := build(456, "epoch-b1")
	thirdDigest, thirdImage := build(456, "epoch-b2")
	if firstDigest == secondDigest {
		t.Fatal("different epochs produced the same manifest")
	}
	if secondDigest != thirdDigest || !reflect.DeepEqual(secondImage, thirdImage) {
		secondManifest, _ := readPlanImage(t, filepath.Join(root, "epoch-b1"))
		thirdManifest, _ := readPlanImage(t, filepath.Join(root, "epoch-b2"))
		t.Fatalf("warm cache changed output for identical SOURCE_DATE_EPOCH: cold annotations=%v warm annotations=%v cold config=%+v warm config=%+v", secondManifest.Annotations, thirdManifest.Annotations, secondImage, thirdImage)
	}
}
