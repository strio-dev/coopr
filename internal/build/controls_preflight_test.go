package build

import (
	"context"
	"strings"
	"testing"

	"coopr/internal/buildah"
)

func TestBuildControlConflictsPrecedeDefinitionReads(t *testing.T) {
	for _, test := range []struct {
		options Options
		want    string
	}{
		{Options{AllPlatforms: true, Platform: "linux/amd64"}, "all-platforms and platform"},
		{Options{AllPlatforms: true, Platforms: []string{"linux/amd64"}}, "all-platforms and platform"},
	} {
		_, err := Run(context.Background(), test.options)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("options=%+v err=%v", test.options, err)
		}
	}
}

func TestDockerAnnotationControlsReachDefinitionValidation(t *testing.T) {
	for _, controls := range []buildah.ImageControls{
		{Annotations: []string{"key=value"}},
		{UnsetAnnotations: []string{"key"}},
		{DropInheritedAnnotations: true},
	} {
		_, err := Run(context.Background(), Options{Format: "docker", ImageControls: controls})
		if err == nil || !strings.Contains(err.Error(), "definition is required") {
			t.Fatalf("Docker annotation options rejected before definition validation: %v", err)
		}
	}
}
