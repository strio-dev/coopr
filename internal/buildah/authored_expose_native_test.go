package buildah

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/openshift/imagebuilder"
	"github.com/strio-dev/coopr/internal/definition"
	"github.com/strio-dev/coopr/internal/imageconfig"
	"github.com/strio-dev/coopr/internal/planner"
)

func TestAuthoredExposeUsesNativeFrontend(t *testing.T) {
	for _, args := range [][]string{
		{"80-82"}, {"83/UDP"}, {"http"}, {"80/custom"}, {"80 443"}, {"80", "443"}, {"83/UDP", "80-82", "83/UDP"},
		{"127.0.0.1:9000:84"}, {"9000-9002:80"}, {"9000-9001:80-81"}, {"[::1]:9000:80"}, {"::1:9000:80"},
		{"0"}, {"65535"}, {"65536"}, {"-1"}, {"90-80"}, {"8000-8002:80-81"}, {"not-an-ip:8000:80"}, {"[::gg]:9000:80"}, {"80/http"}, {"/tcp"}, {"80/tcp/udp"}, nil,
	} {
		t.Run(stringifyExposeArguments(t, args), func(t *testing.T) {
			native := imagebuilder.NewBuilder(nil)
			nativeErr := native.Run(&imagebuilder.Step{Command: "expose", Args: args}, imagebuilder.NoopExecutor, true)
			expected := map[string]struct{}{}
			for port := range native.RunConfig.ExposedPorts {
				expected[string(port)] = struct{}{}
			}
			inst := definition.Instruction{Name: "expose", Arguments: args}
			lowered, _, err := lowerOperation(planner.Operation{Instruction: inst}, nil)
			if (err != nil) != (nativeErr != nil) {
				t.Fatalf("lowering error=%v, native=%v", err, nativeErr)
			}
			logical := imageconfig.New()
			configErr := logical.Apply(inst)
			if (configErr != nil) != (nativeErr != nil) {
				t.Fatalf("config error=%v, native=%v", configErr, nativeErr)
			}
			if nativeErr != nil {
				return
			}
			want := slices.Sorted(maps.Keys(expected))
			got := make([]string, 0, len(lowered))
			for _, op := range lowered {
				got = append(got, string(op.(Expose)))
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("lowered ports=%q, native=%q", got, want)
			}
			checkExposeLogicalConfig(t, logical, expected)
			tracked := imageconfig.New()
			for _, op := range lowered {
				if err := trackConfigOperation(tracked, op); err != nil {
					t.Fatal(err)
				}
			}
			checkExposeLogicalConfig(t, tracked, expected)
		})
	}
}

func stringifyExposeArguments(t *testing.T, args []string) string {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func checkExposeLogicalConfig(t *testing.T, config *imageconfig.Config, want map[string]struct{}) {
	t.Helper()
	data, err := config.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Config struct{ ExposedPorts map[string]struct{} }
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(decoded.Config.ExposedPorts, want) {
		t.Errorf("config ports=%v, native=%v", decoded.Config.ExposedPorts, want)
	}
	restored, err := imageconfig.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	again, err := restored.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(again) {
		t.Fatalf("logical/cache roundtrip changed configuration: %s / %s", data, again)
	}
}
