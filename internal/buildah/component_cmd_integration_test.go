package buildah

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestLocalComponentCommandAuthorshipColdAndWarm(t *testing.T) {
	for _, tc := range []struct {
		name, component, before string
		want                    []string
	}{
		{"nested-caller", `extend { component "./inner.coopr"; }`, `cmd { exec "caller"; }`, []string{"caller"}},
		{"nested-component", `extend { component "./inner-command.coopr"; }`, "", []string{"inner"}},
		{"caller-no-layers", `extend {}`, `cmd { exec "caller"; }`, []string{"caller"}},
		{"component-no-layers", `extend { cmd { exec "component"; }; }`, "", []string{"component"}},
		{"caller", `extend {}`, `cmd { exec "caller"; }`, []string{"caller"}},
		{"component", `extend { cmd { exec "component"; }; }`, "", []string{"component"}},
		{"replacement", `extend { from "scratch" { cmd { exec "replacement"; }; }; }`, `cmd { exec "caller"; }`, []string{"replacement"}},
		{"inherited-replacement", `from "scratch" as="base" { cmd { exec "inherited"; }; }
extend { from "base" {} }`, `cmd { exec "caller"; }`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLocalComponentFixture(t)
			f.options.Lifecycle.NoLayers = tc.name == "caller-no-layers" || tc.name == "component-no-layers"
			f.write(t, "component.coopr", tc.component)
			f.write(t, "inner.coopr", `extend {}`)
			f.write(t, "inner-command.coopr", `extend { cmd { exec "inner"; }; }`)
			f.write(t, "payload", "worker proof")
			source := `from "scratch" { ` + tc.before + `
 component "./component.coopr"
 entrypoint { exec "/bin/echo"; }
}`
			for _, attempt := range []string{"cold", "warm"} {
				result, layout := f.build(t, attempt, source)
				_, image := readPlanImage(t, layout)
				if !reflect.DeepEqual(image.Config.Cmd, tc.want) {
					t.Fatalf("%s CMD=%v, want %v", attempt, image.Config.Cmd, tc.want)
				}
				if !reflect.DeepEqual(image.Config.Entrypoint, []string{"/bin/echo"}) {
					t.Fatalf("%s ENTRYPOINT=%v", attempt, image.Config.Entrypoint)
				}
				if attempt == "warm" && !f.options.Lifecycle.NoLayers && result.CacheStats.Hits == 0 {
					t.Fatalf("warm component cache not used: %+v", result.CacheStats)
				}
			}
			// Two independent ready stages force process-isolated execution; a
			// final dependent stage preserves the tested stage's image config.
			parallelSource := `from "scratch" as="parallel" { copy "payload" "/payload"; }
` + `from "scratch" as="tested" { ` + tc.before + `
 component "./component.coopr"
 entrypoint { exec "/bin/echo"; }
}
from "tested" { copy "/payload" "/payload" from="parallel"; }`
			for _, attempt := range []string{"isolated-cold", "isolated-warm"} {
				options := f.options
				options.Jobs = 2
				options.NoCache = attempt == "isolated-cold"
				options.Output = Output{Path: filepath.Join(f.root, attempt)}
				result, err := BuildPlan(f.ctx, testPlan(t, parallelSource), options)
				if err != nil {
					t.Fatal(err)
				}
				_, image := readPlanImage(t, options.Output.Path)
				if !reflect.DeepEqual(image.Config.Cmd, tc.want) {
					t.Fatalf("%s CMD=%v want %v, cache=%+v", attempt, image.Config.Cmd, tc.want, result.CacheStats)
				}
				if attempt == "isolated-warm" && !options.Lifecycle.NoLayers && result.CacheStats.Hits == 0 {
					t.Fatalf("isolated worker did not restore component cache: %+v", result.CacheStats)
				}
			}
		})
	}
}
