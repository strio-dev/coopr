package planner

import (
	"reflect"
	"testing"
)

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
		"ALL_PROXY":   "socks5://uppercase",
		"all_proxy":   "socks5://lowercase",
	}
	plan := makePlan(t, "from \"scratch\"\nrun \"env\"\n", Options{Arguments: arguments})
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
	plan := makePlan(t, "from \"scratch\"\narg \"HTTP_PROXY\"\nrun \"env\"\n", Options{
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
	plan := makePlan(t, "from \"scratch\"\nenv HTTP_PROXY=\"http://image-env\"\nrun \"env\"\n", Options{
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
	definition := parse(t, "from \"scratch\"\nrun \"true\"\n")
	base := makePlan(t, "from \"scratch\"\nrun \"true\"\n", Options{})
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
