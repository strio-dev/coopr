package planner

import (
	"reflect"
	"testing"

	"github.com/openshift/imagebuilder"
	commonconfig "go.podman.io/common/pkg/config"
)

func TestPredefinedProxyArgumentsFollowNativeList(t *testing.T) {
	arguments := map[string]string{"ALL_PROXY": "socks5://uppercase", "all_proxy": "socks5://lowercase", "OTHER": "value"}
	want := map[string]string{}
	native := imagebuilder.NewBuilder(nil)
	for _, name := range commonconfig.ProxyEnv {
		if !native.AllowedArgs[name] {
			t.Fatalf("native proxy list contains nonautomatic argument %q", name)
		}
		arguments[name] = "proxy-value"
		want[name] = "proxy-value"
	}
	if got := PredefinedProxyArguments(arguments); !reflect.DeepEqual(got, want) {
		t.Fatalf("automatic proxy arguments = %#v, native=%#v", got, want)
	}
}

func TestExplicitAllProxyArgumentUsesOrdinaryScope(t *testing.T) {
	plan := makePlan(t, `from "scratch" {
    arg "ALL_PROXY"
    run "env"
}
`, Options{Arguments: map[string]string{"ALL_PROXY": "socks5://explicit"}})
	operation := plan.Stages[0].Operations[0]
	if got := operation.ArgumentsInScope["ALL_PROXY"]; got != "socks5://explicit" {
		t.Fatalf("explicit ALL_PROXY = %q", got)
	}
	if len(operation.DeclaredProxyArguments) != 0 {
		t.Fatalf("ordinary ALL_PROXY given special proxy handling: %q", operation.DeclaredProxyArguments)
	}
}

func TestPredefinedProxyArgumentsMayBeSuppliedWithoutARG(t *testing.T) {
	arguments := map[string]string{
		"HTTP_PROXY":  "http://uppercase",
		"http_proxy":  "http://lowercase",
		"HTTPS_PROXY": "https://uppercase",
		"https_proxy": "https://lowercase",
		"FTP_PROXY":   "ftp://uppercase",
		"ftp_proxy":   "ftp://lowercase",
		"NO_PROXY":    "uppercase.example",
		"no_proxy":    "lowercase.example",
	}
	plan := makePlan(t, `from "scratch" {
    run "env"
}
`, Options{Arguments: arguments})
	operation := plan.Stages[0].Operations[0]
	if len(operation.ArgumentsInScope) != 0 || len(operation.DeclaredProxyArguments) != 0 {
		t.Fatalf("undeclared proxy arguments entered planned RUN identity: %+v", operation)
	}
	if len(plan.Arguments) != 0 {
		t.Fatalf("undeclared proxy arguments entered public argument metadata: %+v", plan.Arguments)
	}
	if got := PredefinedProxyArguments(arguments); !reflect.DeepEqual(got, arguments) {
		t.Fatalf("predefined proxy filter = %#v, want %#v", got, arguments)
	}
}

func TestExplicitProxyARGUsesNormalArgumentScope(t *testing.T) {
	plan := makePlan(t, `from "scratch" {
    arg "HTTP_PROXY"
    run "env"
}
`, Options{
		Arguments: map[string]string{"HTTP_PROXY": "http://explicit"},
	})
	operation := plan.Stages[0].Operations[0]
	if got := operation.ArgumentsInScope["HTTP_PROXY"]; got != "http://explicit" {
		t.Fatalf("explicit proxy argument = %q", got)
	}
	if !reflect.DeepEqual(operation.DeclaredProxyArguments, []string{"HTTP_PROXY"}) {
		t.Fatalf("declared proxy arguments = %#v", operation.DeclaredProxyArguments)
	}
}

func TestENVShadowsUndeclaredPredefinedProxyArgument(t *testing.T) {
	plan := makePlan(t, `from "scratch" {
    env HTTP_PROXY="http://image-env"
    run "env"
}
`, Options{
		Arguments: map[string]string{"HTTP_PROXY": "http://cli"},
	})
	operation := plan.Stages[0].Operations[1]
	if !reflect.DeepEqual(operation.ShadowedProxyArguments, []string{"HTTP_PROXY"}) {
		t.Fatalf("shadowed proxy arguments = %#v", operation.ShadowedProxyArguments)
	}
	if _, present := operation.ArgumentsInScope["HTTP_PROXY"]; present {
		t.Fatalf("ENV-shadowed undeclared proxy entered ARG scope: %#v", operation.ArgumentsInScope)
	}
}

func TestUnknownUndeclaredBuildArgumentIsIgnored(t *testing.T) {
	definition := parse(t, `from "scratch" {
    run "true"
}
`)
	base := makePlan(t, `from "scratch" {
    run "true"
}
`, Options{})
	plan, err := Create(definition, Options{
		Arguments: map[string]string{"TYPO_PROXY": "http://invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan, base) {
		t.Fatalf("unused build argument changed the plan: %#v", plan)
	}
}
