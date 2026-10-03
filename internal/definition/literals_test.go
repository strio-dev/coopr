package definition

import (
	"reflect"
	"strings"
	"testing"
)

func TestHealthcheckNONEKeyword(t *testing.T) {
	def, err := Parse(strings.NewReader(`
healthcheck NONE
healthcheck "NONE"
healthcheck { exec "NONE" }
onbuild { healthcheck NONE }
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Instruction{
		{Name: "healthcheck", Arguments: []string{"NONE"}},
		{Name: "healthcheck", Arguments: []string{"CMD-SHELL", "NONE"}},
		{Name: "healthcheck", Arguments: []string{"CMD", "NONE"}},
		{Name: "onbuild", Arguments: []string{"HEALTHCHECK NONE"}},
	}
	if !reflect.DeepEqual(def.Instructions, want) {
		t.Fatalf("healthcheck forms = %#v, want %#v", def.Instructions, want)
	}
	if err := Validate(def); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{
		`healthcheck NONE "extra"`,
		`healthcheck NONE interval="30s"`,
		`healthcheck NONE { exec "/check" }`,
		`healthcheck #null`,
		`healthcheck { exec #null }`,
	} {
		if _, err := Parse(strings.NewReader(source)); err == nil {
			t.Errorf("accepted invalid healthcheck %s", source)
		}
	}
}

func TestQuotedBooleanOptionsMatchNativeValues(t *testing.T) {
	for _, source := range []string{
		`copy "src" "/dst" link="true" parents="false"`,
		`add "archive.tar" "/dst" unpack="false" keep-git-dir="true"`,
		`run "true" { mount "bind" target="/input" readonly="true" }`,
		`run "true" { mount "secret" id="token" required="false" }`,
		`run "true" { device "vendor.example/gpu=one" required="true" }`,
		`run "true" { device "vendor.example/gpu=one" required="false" }`,
	} {
		quoted, err := Parse(strings.NewReader(source))
		if err != nil {
			t.Fatal(err)
		}
		nativeSource := strings.NewReplacer(`="true"`, `=#true`, `="false"`, `=#false`).Replace(source)
		native, err := Parse(strings.NewReader(nativeSource))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(quoted, native) {
			t.Fatalf("quoted options differ from native values for %s", source)
		}
		if err := Validate(quoted); err != nil {
			t.Fatal(err)
		}
	}
	for _, source := range []string{
		`copy "src" "/dst" link=true`,
		`copy "src" "/dst" link=false`,
		`run "true" { device "vendor.example/gpu=one" required="yes" }`,
		`run "true" { device "vendor.example/gpu=one" required=1 }`,
		`run "true" { device "vendor.example/gpu=one" required=(flag)"true" }`,
	} {
		if _, err := Parse(strings.NewReader(source)); err == nil {
			t.Errorf("accepted invalid boolean option %s", source)
		}
	}
}
