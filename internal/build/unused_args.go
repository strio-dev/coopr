package build

import (
	"fmt"
	"io"
	"slices"

	"coopr/internal/definition"
	"coopr/internal/planner"
)

// Buildah treats an ARG declaration anywhere in the definition as consumption,
// including unused stages. ONBUILD declarations belong to the next build.
func warnUnusedBuildArguments(writer io.Writer, def *definition.Definition, arguments map[string]string) {
	if writer == nil {
		writer = io.Discard
	}
	unused := make(map[string]bool, len(arguments))
	for name := range arguments {
		if planner.IsPredefinedProxyArgument(name) {
			continue
		}
		switch name {
		case "SOURCE_DATE_EPOCH", "BUILDPLATFORM", "BUILDOS", "BUILDOSVERSION", "BUILDARCH", "BUILDVARIANT", "TARGETPLATFORM", "TARGETOS", "TARGETOSVERSION", "TARGETARCH", "TARGETVARIANT", "TARGETSTAGE":
			continue
		}
		unused[name] = true
	}
	var declarations func([]definition.Instruction)
	declarations = func(instructions []definition.Instruction) {
		for _, inst := range instructions {
			if inst.Name == "arg" && len(inst.Arguments) > 0 {
				delete(unused, inst.Arguments[0])
			}
			if inst.Name == "layer" {
				declarations(inst.Children)
			}
		}
	}
	declarations(def.Instructions)
	if len(unused) == 0 {
		return
	}
	names := make([]string, 0, len(unused))
	for name := range unused {
		names = append(names, name)
	}
	slices.Sort(names)
	_, _ = fmt.Fprintf(writer, "[Warning] one or more build args were not consumed: %v\n", names)
}
