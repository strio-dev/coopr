package apibackend

import (
	"encoding/json"
	"fmt"

	"github.com/strio-dev/coopr/api"
)

func applyNativeResultMetadata(result *api.Result, data []byte) error {
	if err := json.Unmarshal(data, &result.Metadata); err != nil {
		return err
	}
	_ = json.Unmarshal(result.Metadata["containerimage.digest"], &result.ManifestDigest)
	if raw, exists := result.Metadata["coopr.platforms"]; exists {
		var selected []struct {
			Platform string `json:"platform"`
		}
		if err := json.Unmarshal(raw, &selected); err != nil {
			return fmt.Errorf("decode native result platforms: %w", err)
		}
		result.Platform = ""
		if len(selected) == 1 {
			platform, err := normalizeBuildPlatform(selected[0].Platform)
			if err != nil || platform == "" {
				return fmt.Errorf("native result has invalid actual platform %q", selected[0].Platform)
			}
			result.Platform = platform
		}
	}
	// The bounded trace belongs in CLI metadata and the streamed observations.
	// Duplicating it here could exceed the terminal event limit after success.
	delete(result.Metadata, "coopr.operation.progress")
	delete(result.Metadata, "coopr.operation.progress.dropped")
	return nil
}
