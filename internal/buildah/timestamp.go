package buildah

import (
	"errors"
	"time"

	upstream "go.podman.io/buildah"
)

func validateTimestampOptions(timestamp, sourceDateEpoch *int64, rewrite bool) error {
	if timestamp != nil && sourceDateEpoch != nil {
		return errors.New("timestamp and source date epoch are mutually exclusive")
	}
	if timestamp != nil && rewrite {
		return errors.New("rewrite timestamp cannot be combined with timestamp")
	}
	return nil
}

type timestampPolicy struct {
	timestamp        *int64
	sourceDateEpoch  *int64
	rewriteTimestamp bool
}

func (p timestampPolicy) apply(options *upstream.CommitOptions) {
	if p.timestamp != nil {
		value := time.Unix(*p.timestamp, 0).UTC()
		options.HistoryTimestamp = &value
	}
	if p.sourceDateEpoch != nil {
		value := time.Unix(*p.sourceDateEpoch, 0).UTC()
		options.SourceDateEpoch = &value
	}
	options.RewriteTimestamp = p.rewriteTimestamp
}

func timestampPolicyFromOptions(options PlanOptions) timestampPolicy {
	return timestampPolicy{timestamp: options.Timestamp, sourceDateEpoch: options.SourceDateEpoch, rewriteTimestamp: options.RewriteTimestamp}
}

func (p timestampPolicy) createdEpoch() *int64 {
	if p.timestamp != nil {
		return p.timestamp
	}
	return p.sourceDateEpoch
}
