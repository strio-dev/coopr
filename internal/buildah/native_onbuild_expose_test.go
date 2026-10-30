package buildah

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/onbuildparse"
	"github.com/strio-dev/coopr/internal/planner"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestImportedExposeBackendKeepsNativeToken(t *testing.T) {
	for _, test := range []struct {
		arguments []string
		want      []Operation
	}{
		{[]string{"80", "443"}, []Operation{Expose("443/tcp"), Expose("80/tcp")}},
		{[]string{"80 443"}, []Operation{Expose("80 443/tcp")}},
		{[]string{"http"}, []Operation{Expose("http/tcp")}},
	} {
		got, _, err := lowerOperation(planner.Operation{Instruction: definition.Instruction{Name: "expose", Arguments: test.arguments}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("operations %#v, want %#v", got, test.want)
		}
		logical := imageconfig.New()
		for _, operation := range got {
			if err := trackConfigOperation(logical, operation); err != nil {
				t.Fatal(err)
			}
		}
		encoded, err := logical.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Config struct {
				ExposedPorts map[string]struct{} `json:"ExposedPorts"`
			} `json:"config"`
		}
		if err := json.Unmarshal(encoded, &config); err != nil {
			t.Fatal(err)
		}
		for _, operation := range test.want {
			if _, ok := config.Config.ExposedPorts[string(operation.(Expose))]; !ok {
				t.Fatalf("logical ports %#v lost canonical operation %#v", config.Config.ExposedPorts, operation)
			}
		}
	}
}

func TestImportedExposeRootlessImage(t *testing.T) {
	if testing.Short() {
		t.Skip("live rootless image build")
	}
	root := t.TempDir()
	def, err := definition.Parse(strings.NewReader(`from "scratch" { expose "80-82" "83/UDP" "http" "80/custom"; expose "80 443"; }`))
	if err != nil {
		t.Fatal(err)
	}
	var inherited []definition.Instruction
	for _, raw := range []string{`EXPOSE $PORTS`, `EXPOSE "$PORTS"`} {
		parsed, err := onbuildparse.ParseDeferred(raw)
		if err != nil {
			t.Fatal(err)
		}
		inherited = append(inherited, parsed...)
	}
	plan, err := planner.CreateDemandDriven(def, planner.Options{Mode: planner.Build, StageBinds: map[string]planner.StageBind{"0": {BaseEnvironment: map[string]string{"PORTS": "80 443"}, Inherited: inherited}}}, func(source planner.FromSource) (planner.StageBind, error) {
		return planner.StageBind{}, fmt.Errorf("unexpected image resolution: %s", source.Source)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Plans cross worker/publication/cache boundaries as JSON. Imported token
	// payloads must survive that transport before native execution.
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var restored planner.Plan
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	plan = &restored
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	layout := filepath.Join(root, "output")
	_, err = BuildPlan(ctx, plan, PlanOptions{Store: StoreOptions{RunRoot: filepath.Join(root, "run"), GraphRoot: filepath.Join(root, "graph"), GraphDriverName: "vfs"}, ContextDir: root, Isolation: "rootless", Output: Output{Path: layout, Format: outputFormatDocker}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := oci.ReadImageConfigLayout(ctx, layout)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Config struct {
			ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		} `json:"config"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{}{"80/tcp": {}, "443/tcp": {}, "80 443/tcp": {}, "80-82/tcp": {}, "83/UDP": {}, "http/tcp": {}, "80/custom": {}}
	if !reflect.DeepEqual(config.Config.ExposedPorts, want) {
		t.Fatalf("ports %#v, want %#v", config.Config.ExposedPorts, want)
	}
}
