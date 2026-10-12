package definition

import (
	"fmt"
	"regexp"
	"strings"
)

var componentArgumentName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ComponentArguments validates and extracts invocation bindings. Bindings remain
// children in the authored definition and do not declare arguments in its stage.
func (inst Instruction) ComponentArguments() (map[string]string, error) {
	if inst.Name != "component" || len(inst.Arguments) != 1 || strings.TrimSpace(inst.Arguments[0]) == "" || strings.ContainsRune(inst.Arguments[0], '\x00') || len(inst.Properties) != 0 {
		return nil, fmt.Errorf("expected one component reference, no properties, and optional arg children")
	}
	parameters := make(map[string]string, len(inst.Children))
	for i, binding := range inst.Children {
		if binding.Name != "arg" || len(binding.Arguments) != 2 || len(binding.Properties) != 0 || len(binding.Children) != 0 || binding.Form != "" || binding.Commands != nil || len(binding.InlineFiles) != 0 || binding.LayerBoundary != "" || binding.ProcessQuotes || binding.DeferredOnBuild != "" {
			return nil, fmt.Errorf("component child %d: expected arg with a name, value, and no properties or children", i+1)
		}
		name, value := binding.Arguments[0], binding.Arguments[1]
		if !componentArgumentName.MatchString(name) {
			return nil, fmt.Errorf("component child %d: invalid argument name %q", i+1, name)
		}
		if strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("component argument %q cannot contain NUL", name)
		}
		if _, exists := parameters[name]; exists {
			return nil, fmt.Errorf("duplicate component argument %q", name)
		}
		parameters[name] = value
	}
	return parameters, nil
}
