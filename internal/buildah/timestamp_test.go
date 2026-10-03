package buildah

import (
	"testing"
	"time"

	upstream "go.podman.io/buildah"
)

func TestTimestampPolicyDistinguishesForceAndSourceDateEpoch(t *testing.T) {
	forced := int64(123)
	options := upstream.CommitOptions{}
	(timestampPolicy{timestamp: &forced}).apply(&options)
	if options.HistoryTimestamp == nil || !options.HistoryTimestamp.Equal(time.Unix(forced, 0)) || options.SourceDateEpoch != nil {
		t.Fatalf("forced timestamp options = %#v", options)
	}
	epoch := int64(456)
	options = upstream.CommitOptions{}
	(timestampPolicy{sourceDateEpoch: &epoch, rewriteTimestamp: true}).apply(&options)
	if options.SourceDateEpoch == nil || !options.SourceDateEpoch.Equal(time.Unix(epoch, 0)) || options.HistoryTimestamp != nil || !options.RewriteTimestamp {
		t.Fatalf("source-date-epoch options = %#v", options)
	}
	if err := validateTimestampOptions(&forced, &epoch, false); err == nil {
		t.Fatal("timestamp and source-date-epoch conflict was accepted")
	}
}
