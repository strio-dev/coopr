package build

import "coopr/internal/definition"

// applyFromOverride implements Buildah's --from behavior: only the image of
// the first FROM instruction is replaced. Later stages retain their authored
// bases.
func applyFromOverride(def *definition.Definition, from string) {
	if def == nil || from == "" {
		return
	}
	for i := range def.Instructions {
		instruction := &def.Instructions[i]
		if instruction.Name == "from" && len(instruction.Arguments) != 0 {
			instruction.Arguments[0] = from
			return
		}
	}
}
