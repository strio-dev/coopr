package planner

import (
	"slices"
	"strings"

	"coopr/internal/definition"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
)

var predefinedProxyArgumentNames = map[string]bool{
	"HTTP_PROXY": true, "http_proxy": true,
	"HTTPS_PROXY": true, "https_proxy": true,
	"FTP_PROXY": true, "ftp_proxy": true,
	"NO_PROXY": true, "no_proxy": true,
	"ALL_PROXY": true, "all_proxy": true,
}

// IsPredefinedProxyArgument reports whether name has Dockerfile's special
// proxy build-argument semantics.
func IsPredefinedProxyArgument(name string) bool {
	return predefinedProxyArgumentNames[name]
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
	collect := func(value string, processQuotes bool) {
		lex := shell.NewLex('\\')
		lex.SkipProcessQuotes = !processQuotes
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
		collect(value, inst.ProcessQuotes)
	}
	for _, value := range inst.Properties {
		collect(value, inst.ProcessQuotes)
	}
	for _, file := range inst.InlineFiles {
		if file.Expand {
			collect(file.Data, false)
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
	return expandWord(s, values, true)
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
	return expand(s, values)
}

func automaticPlatformScope(opts Options) scope {
	targetOS, targetArch, targetVariant := splitPlatform(opts.Platform)
	build := platforms.DefaultSpec()
	targetStage := opts.Target
	if targetStage == "" {
		targetStage = "default"
	}
	values := scope{
		"BUILDPLATFORM":   {value: platforms.Format(build), present: true},
		"BUILDOS":         {value: build.OS, present: true},
		"BUILDOSVERSION":  {value: build.OSVersion, present: true},
		"BUILDARCH":       {value: build.Architecture, present: true},
		"BUILDVARIANT":    {value: build.Variant, present: true},
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
	parts := strings.Split(platform, "/")
	os, architecture = parts[0], parts[1]
	if len(parts) == 3 {
		variant = parts[2]
	}
	return os, architecture, variant
}
