package definition_test

import (
	"io"
	"strings"

	"github.com/strio-dev/coopr/internal/definition"
)

func parseFragment(r io.Reader) (*definition.Definition, error) {
	def, err := definition.Parse(io.MultiReader(strings.NewReader("from \"scratch\" {\n"), r, strings.NewReader("\n}")))
	if err != nil {
		return nil, err
	}
	return &definition.Definition{Instructions: def.Instructions[0].Children}, nil
}
