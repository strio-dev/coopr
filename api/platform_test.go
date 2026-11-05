package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildPlatformWireContract(t *testing.T) {
	for _, platform := range []string{"", "linux/arm64"} {
		data, err := json.Marshal(Build{Files: []string{"image.coopr"}, Platform: platform})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `"platform"`) != (platform != "") {
			t.Fatalf("optional platform field: %s", data)
		}
		var decoded Build
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Platform != platform {
			t.Fatalf("platform lost: %+v", decoded)
		}
	}
}
