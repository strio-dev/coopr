package definition

import (
	"io"
	"strings"
)

// Operation tests use a real stage wrapper; Parse itself accepts only complete
// documents. The returned fragment keeps operation assertions focused.
func parseFragment(r io.Reader) (*Definition, error) {
	def, err := Parse(io.MultiReader(strings.NewReader("from \"scratch\" {\n"), r, strings.NewReader("\n}")))
	if err != nil {
		return nil, err
	}
	return &Definition{Instructions: def.Instructions[0].Children}, nil
}

func validateFragment(def *Definition) error {
	if def == nil || len(def.Instructions) == 0 {
		return Validate(def)
	}
	return Validate(&Definition{Instructions: []Instruction{{Name: "from", Arguments: []string{"scratch"}, Children: def.Instructions}}})
}
