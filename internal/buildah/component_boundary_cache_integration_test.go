package buildah

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// Component boundaries must not turn wall-clock commit metadata into a RUN
// dependency. Exercise real worker/storage cache hits without timestamp controls.
func TestComponentBoundaryInstructionCacheWithoutTimestamp(t *testing.T) {
	requireLiveInstructionCache(t)
	for _, kind := range []string{"reusable", "local"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			root := t.TempDir()
			store := cacheTestStore(root)
			base := newLiveBusyBoxStorage(t, ctx, root, store)
			component := "extend\nenv COMPONENT_BOUNDARY=\"yes\"\n"
			reference := "local:config"
			if kind == "reusable" {
				_ = localConfigComponentResolver(t, ctx, root, component, false)
			} else {
				reference = "./component.coopr"
				if err := os.WriteFile(filepath.Join(root, "component.coopr"), []byte(component), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			policy := writeComponentTestPolicy(t, root)
			type output struct {
				proof string
				image v1.Image
				hit   bool
			}
			build := func(name, label, env, payload string) output {
				t.Helper()
				payloadPath := filepath.Join(root, "payload")
				if existing, err := os.ReadFile(payloadPath); err != nil || string(existing) != payload {
					if err := os.WriteFile(payloadPath, []byte(payload), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				plan := testPlan(t, fmt.Sprintf(`from %q
copy "payload" "/input"
label "org.example.boundary" %q
env CALLER_ENV=%q
component %q
run "od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' >/proof; cat /input >>/proof; printf '%%s' \"$CALLER_ENV\" >>/proof" network="none"
`, base.reference, label, env, reference))
				if plan.SourceDateEpoch != nil {
					t.Fatal("regression requires an unpinned source date epoch")
				}
				layout := filepath.Join(root, name)
				var logs strings.Builder
				_, err := BuildPlanSupervised(ctx, plan, SupervisedPlanOptions{
					Store: store, ContextDir: root, Isolation: "rootless", Runtime: "crun",
					Output:            Output{Path: layout},
					ComponentStoreDir: filepath.Join(root, "components"),
					CacheLocalDir:     filepath.Join(root, "cache"), SignaturePolicyPath: policy,
					Stdout: io.Discard, Stderr: &logs,
				})
				t.Logf("%s worker progress:\n%s", name, logs.String())
				if err != nil {
					t.Fatal(err)
				}
				_, image := readPlanImage(t, layout)
				// Earlier COPY/component hits do not prove that the random RUN hit.
				runProgress := strings.LastIndex(logs.String(), ": RUN ")
				hit := runProgress >= 0 && strings.Contains(logs.String()[runProgress:], "--> Using cache ")
				return output{proof: localComponentLastFile(t, layout, "proof"), image: image, hit: hit}
			}
			cold := build("cold", "one", "one", "payload-one")
			// Cross a wall-clock second so a timestamp-sensitive boundary is exposed.
			time.Sleep(1100 * time.Millisecond)
			warm := build("warm", "one", "one", "payload-one")
			if !warm.hit || warm.proof != cold.proof {
				t.Errorf("warm component-boundary RUN reexecuted: hit=%v cold=%q warm=%q", warm.hit, cold.proof, warm.proof)
			}
			metadata := build("label-change", "two", "one", "payload-one")
			if !metadata.hit || metadata.proof != cold.proof {
				t.Errorf("label-only change reexecuted component-boundary RUN: hit=%v cold=%q changed=%q", metadata.hit, cold.proof, metadata.proof)
			}
			if got := metadata.image.Config.Labels["org.example.boundary"]; got != "two" {
				t.Errorf("cached output label = %q, want two", got)
			}
			var history strings.Builder
			for _, entry := range metadata.image.History {
				history.WriteString(entry.CreatedBy + "\n")
			}
			if !strings.Contains(history.String(), "LABEL org.example.boundary=two") || strings.Contains(history.String(), "LABEL org.example.boundary=one") {
				t.Errorf("cached output did not retain current caller history:\n%s", history.String())
			}
			for _, change := range []struct{ name, env, payload string }{
				{"environment-change", "two", "payload-one"},
				{"filesystem-change", "one", "payload-two"},
			} {
				changed := build(change.name, "one", change.env, change.payload)
				if changed.hit || changed.proof == cold.proof {
					t.Errorf("%s reused stale RUN: hit=%v cold=%q changed=%q", change.name, changed.hit, cold.proof, changed.proof)
				}
				if !strings.Contains(changed.proof, change.payload+change.env) {
					t.Errorf("%s proof %q does not reflect caller inputs", change.name, changed.proof)
				}
			}
		})
	}
}
