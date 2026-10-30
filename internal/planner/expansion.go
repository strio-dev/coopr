package planner

import (
	"slices"

	"github.com/strio-dev/coopr/internal/definition"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	"github.com/openshift/imagebuilder"
	commonconfig "go.podman.io/common/pkg/config"
)

// IsPredefinedProxyArgument reports whether name has Dockerfile's special
// proxy build-argument semantics.
func IsPredefinedProxyArgument(name string) bool {
	return slices.Contains(commonconfig.ProxyEnv, name)
}

// PredefinedProxyArguments returns only Dockerfile predefined proxy values.
// The returned map is detached from the caller's argument map.
func PredefinedProxyArguments(arguments map[string]string) map[string]string {
	result := map[string]string{}
	for name, value := range arguments {
		if IsPredefinedProxyArgument(name) {
			result[name] = value
		}
	}
	return result
}

func sortedTrueKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key, present := range values {
		if present {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

func (g *graph) collectArgumentReferences(inst definition.Instruction, values scope, used map[string]bool) {
	// These configuration values are literal in native Dockerfile semantics.
	if inst.Name == "shell" || inst.Name == "maintainer" {
		return
	}
	if inst.Name == "healthcheck" && len(inst.Arguments) == 1 && inst.Arguments[0] == "NONE" {
		return // Native NONE ignores options, including references in their values.
	}
	collect := func(value string, processQuotes, heredoc bool) {
		lex := shell.NewLex('\\')
		lex.SkipProcessQuotes = !processQuotes
		if heredoc {
			lex.SkipProcessQuotes = false
			lex.RawQuotes = true
			lex.RawEscapes = true
		}
		result, err := lex.ProcessWordWithMatches(value, expansionEnv(values))
		if err != nil {
			return
		}
		for name := range result.Matched {
			if value, declared := values[name]; declared && value.present {
				used[name] = true
			}
		}
	}
	start := 0
	if inst.Name == "arg" {
		start = 1
	}
	for _, value := range inst.Arguments[start:] {
		collect(value, inst.ProcessQuotes, false)
	}
	for _, command := range inst.Commands {
		for _, value := range command.Arguments {
			collect(value, inst.ProcessQuotes, false)
		}
	}
	for _, value := range inst.Properties {
		collect(value, inst.ProcessQuotes, false)
	}
	for _, file := range inst.InlineFiles {
		if file.Expand {
			collect(file.Data, false, true)
		}
	}
	for _, child := range inst.Children {
		g.collectArgumentReferences(child, values, used)
	}
}

// expansionEnv is the narrow adapter between planner argument scope and
// BuildKit's Dockerfile-compatible variable expansion. Keeping this adapter in
// the planner avoids importing any BuildKit frontend, client, or LLB surface.
type expansionEnv scope

func (e expansionEnv) Get(name string) (string, bool) {
	value, ok := e[name]
	return value.value, ok && value.present
}

func (e expansionEnv) Keys() []string {
	keys := make([]string, 0, len(e))
	for name, value := range e {
		if value.present {
			keys = append(keys, name)
		}
	}
	slices.Sort(keys)
	return keys
}

func expand(s string, values scope) (string, error) {
	return expandWord(s, values, false)
}

func expandDockerWord(s string, values scope) (string, error) {
	return imagebuilder.ProcessWord(s, dockerEnvironment(values))
}

func expandWord(s string, values scope, processQuotes bool) (string, error) {
	lex := shell.NewLex('\\')
	// Definition parsing has already removed the KDL string delimiters. Quotes
	// in authored KDL are payload bytes. Imported Dockerfile ONBUILD words retain
	// their lexical quotes until child-stage expansion and process them once.
	lex.SkipProcessQuotes = !processQuotes
	result, err := lex.ProcessWordWithMatches(s, expansionEnv(values))
	if err != nil {
		return "", err
	}
	return result.Result, nil
}

func expandHeredoc(s string, values scope) (string, error) {
	lex := shell.NewLex('\\')
	lex.RawQuotes = true
	lex.RawEscapes = true
	result, err := lex.ProcessWordWithMatches(s, expansionEnv(values))
	if err != nil {
		return "", err
	}
	return result.Result, nil
}

func automaticPlatformScope(opts Options) scope {
	targetOS, targetArch, targetVariant := splitPlatform(opts.Platform)
	build := platforms.DefaultSpec()
	// Buildah retains imagebuilder's host build defaults for every target platform.
	buildDefaults := imagebuilder.NewBuilder(nil).BuiltinArgDefaults
	targetStage := opts.Target
	if targetStage == "" {
		targetStage = "default"
	}
	values := scope{
		"BUILDPLATFORM":   {value: buildDefaults["BUILDPLATFORM"], present: true},
		"BUILDOS":         {value: buildDefaults["BUILDOS"], present: true},
		"BUILDOSVERSION":  {value: build.OSVersion, present: true},
		"BUILDARCH":       {value: buildDefaults["BUILDARCH"], present: true},
		"BUILDVARIANT":    {value: buildDefaults["BUILDVARIANT"], present: true},
		"TARGETPLATFORM":  {value: opts.Platform, present: true},
		"TARGETOS":        {value: targetOS, present: true},
		"TARGETOSVERSION": {value: "", present: true},
		"TARGETARCH":      {value: targetArch, present: true},
		"TARGETVARIANT":   {value: targetVariant, present: true},
		"TARGETSTAGE":     {value: targetStage, present: true},
	}
	for name, value := range opts.Arguments {
		if _, automatic := values[name]; automatic {
			values[name] = binding{value: value, present: true}
		}
	}
	return values
}

func splitPlatform(platform string) (os, architecture, variant string) {
	parsed, _ := platforms.Parse(platform) // Planner entry points validate this value.
	return parsed.OS, parsed.Architecture, parsed.Variant
}

func dockerEnvironment(values scope) []string {
	env := make([]string, 0, len(values))
	for _, name := range sortedKeys(values) {
		if values[name].present {
			env = append(env, name+"="+values[name].value)
		}
	}
	return env
}
