package buildah

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/planner"
)

func TestExtendPreservesCallerCommandAuthorship(t *testing.T) {
	caller := imageconfig.New()
	if err := caller.Apply(definition.Instruction{Name: "cmd", Arguments: []string{"hello"}}); err != nil {
		t.Fatal(err)
	}
	executor := &graphExecutor{}
	prepared, err := executor.prepareGraphStage(context.Background(), &planner.Plan{Mode: planner.Invoke}, planner.Stage{ID: "extend", Kind: "extend", Platform: "linux/amd64"}, nil, nil, &graphBindings{cacheScope: CacheMountScope{Kind: "component", Source: digest.FromString("test"), Platform: v1.Platform{OS: "linux", Architecture: "amd64"}}, caller: stageState{storageImageID: "caller", config: caller}})
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.logical.Apply(definition.Instruction{Name: "entrypoint", Arguments: []string{"/bin/echo"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := prepared.logical.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var image v1.Image
	if err := json.Unmarshal(raw, &image); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(image.Config.Cmd, []string{"hello"}) {
		t.Fatalf("EXTEND lost same-stage CMD: %v", image.Config.Cmd)
	}
}
