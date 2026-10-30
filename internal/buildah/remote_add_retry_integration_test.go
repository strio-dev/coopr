package buildah

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestSupervisedHTTPAddHonorsRetryOption(t *testing.T) {
	requireLiveInstructionCache(t)
	for _, retries := range []uint{0, 1} {
		t.Run(strconv.Itoa(int(retries)), func(t *testing.T) {
			fixture := newLocalComponentFixture(t)
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if attempts.Add(1) == 1 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				_, _ = w.Write([]byte("retried payload"))
			}))
			defer server.Close()
			plan := testPlan(t, fmt.Sprintf("from \"scratch\" { add %q \"/proof\"; }", server.URL+"/payload"))
			_, err := BuildPlanSupervised(fixture.ctx, plan, SupervisedPlanOptions{
				Store: fixture.options.Store, ContextDir: fixture.contextDir,
				Isolation: "rootless", Runtime: "crun",
				Output: Output{Path: filepath.Join(fixture.root, "result")},
				Retry:  retries, RetrySet: true, RetryDelay: time.Millisecond,
			})
			if retries == 0 {
				if err == nil || attempts.Load() != 1 {
					t.Fatalf("disabled retries: attempts=%d error=%v", attempts.Load(), err)
				}
			} else if err != nil || attempts.Load() != 2 {
				t.Fatalf("supervised retry: attempts=%d error=%v", attempts.Load(), err)
			}
		})
	}
}
