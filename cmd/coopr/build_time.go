package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	buildahcli "go.podman.io/buildah/pkg/cli"
)

type buildTimeFlags struct {
	buildahcli.BudResults
}

func (f *buildTimeFlags) addTo(cmd *cobra.Command) {
	native := buildahcli.GetBudFlags(&f.BudResults)
	addNativeFlags(cmd, &native, buildahcli.GetBudFlagsCompletions(), "cache-ttl", "timestamp", "source-date-epoch")
}

func (f buildTimeFlags) values(cmd *cobra.Command) (timestamp, epoch *int64, ttl *time.Duration, err error) {
	if cmd.Flags().Changed("cache-ttl") {
		duration, parseErr := time.ParseDuration(f.CacheTTL)
		if parseErr != nil {
			return nil, nil, nil, fmt.Errorf("invalid cache-ttl %q: %w", f.CacheTTL, parseErr)
		}
		if duration == 0 && cmd.Flags().Changed("no-cache") {
			noCache, err := cmd.Flags().GetBool("no-cache")
			if err != nil {
				return nil, nil, nil, err
			}
			if !noCache {
				return nil, nil, nil, fmt.Errorf("cannot use --cache-ttl with duration as 0 and --no-cache=false")
			}
		}
		ttl = &duration
	}
	if cmd.Flags().Changed("timestamp") {
		timestamp = &f.Timestamp
	}
	if f.SourceDateEpoch != "" {
		parsed, parseErr := strconv.ParseInt(f.SourceDateEpoch, 10, 64)
		if parseErr != nil {
			return nil, nil, nil, fmt.Errorf("invalid SOURCE_DATE_EPOCH %q: %w", f.SourceDateEpoch, parseErr)
		}
		epoch = &parsed
	}
	if timestamp != nil && epoch != nil {
		return nil, nil, nil, fmt.Errorf("timestamp and source date epoch are mutually exclusive")
	}
	return timestamp, epoch, ttl, nil
}
