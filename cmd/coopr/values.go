package main

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

type namedValues struct {
	values map[string]string
}

func (v *namedValues) Set(value string) error {
	name, val, ok := strings.Cut(value, "=")
	if name == "" {
		return fmt.Errorf("expected a nonempty argument name")
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("argument name must be valid UTF-8")
	}
	if strings.TrimSpace(name) != name || strings.ContainsAny(name, " \t\r\n") {
		return fmt.Errorf("invalid argument name %q", name)
	}
	if !ok {
		var exists bool
		val, exists = os.LookupEnv(name)
		if !exists {
			delete(v.values, name)
			return nil
		}
	}
	if !utf8.ValidString(val) {
		return fmt.Errorf("argument value must be valid UTF-8")
	}
	v.values[name] = val
	return nil
}
