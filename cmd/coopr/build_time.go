package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

type buildTimeFlags struct {
	cacheTTL                   string
	timestamp, sourceDateEpoch int64
}

func (f *buildTimeFlags) addTo(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.StringVar(&f.cacheTTL, "cache-ttl", "", "only read cache results younger than DURATION (0 disables reads)")
	flags.Int64Var(&f.timestamp, "timestamp", 0, "set image and all newly committed file timestamps to epoch seconds")
	flags.Int64Var(&f.sourceDateEpoch, "source-date-epoch", 0, "set the image creation timestamp to epoch seconds")
	cmd.MarkFlagsMutuallyExclusive("timestamp", "source-date-epoch")
	cmd.MarkFlagsMutuallyExclusive("timestamp", "rewrite-timestamp")
}

func (f buildTimeFlags) values(cmd *cobra.Command) (timestamp, epoch *int64, ttl *time.Duration, err error) {
	if cmd.Flags().Changed("cache-ttl") {
		duration, parseErr := time.ParseDuration(f.cacheTTL)
		if parseErr != nil || duration < 0 {
			return nil, nil, nil, fmt.Errorf("invalid cache-ttl %q: expected a nonnegative duration", f.cacheTTL)
		}
		ttl = &duration
	}
	if cmd.Flags().Changed("timestamp") {
		if f.timestamp < 0 {
			return nil, nil, nil, fmt.Errorf("timestamp must be nonnegative")
		}
		timestamp = &f.timestamp
	}
	if cmd.Flags().Changed("source-date-epoch") {
		if f.sourceDateEpoch < 0 {
			return nil, nil, nil, fmt.Errorf("source-date-epoch must be nonnegative")
		}
		epoch = &f.sourceDateEpoch
	} else if value, ok := os.LookupEnv("SOURCE_DATE_EPOCH"); ok {
		parsed, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil || parsed < 0 {
			return nil, nil, nil, fmt.Errorf("invalid SOURCE_DATE_EPOCH %q: expected nonnegative epoch seconds", value)
		}
		epoch = &parsed
	}
	return timestamp, epoch, ttl, nil
}
