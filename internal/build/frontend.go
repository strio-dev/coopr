package build

import "github.com/strio-dev/coopr/internal/definition"

// applyFromOverride implements Buildah's --from behavior: only the image of
// the first FROM instruction is replaced. Later stages retain their authored
// bases.
func applyFromOverride(def *definition.Definition, from string) {
	if def == nil || from == "" {
		return
	}
	var replaceFirst func([]definition.Instruction) bool
	replaceFirst = func(instructions []definition.Instruction) bool {
		for i := range instructions {
			instruction := &instructions[i]
			if instruction.Name == "from" && len(instruction.Arguments) != 0 {
				instruction.Arguments[0] = from
				return true
			}
			if instruction.Name == "extend" && replaceFirst(instruction.Children) {
				return true
			}
		}
		return false
	}
	replaceFirst(def.Instructions)
}
