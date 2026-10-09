package definition

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	buildkitinstructions "github.com/moby/buildkit/frontend/dockerfile/instructions"
	buildkitparser "github.com/moby/buildkit/frontend/dockerfile/parser"
)

// formatOnBuild converts one parsed Coopr instruction to the Dockerfile
// instruction syntax stored in Docker image configuration's OnBuild field. It is a
// metadata boundary only; Coopr never feeds this output to its builder.
func formatOnBuild(inst Instruction) (string, error) {
	raw, err := formatOnBuildInstruction(inst)
	if err != nil {
		return "", err
	}
	// Validate the actual metadata dialect at publication, rather than leaving
	// the consuming build to discover a trigger that Dockerfile cannot parse.
	parsed, err := buildkitparser.Parse(strings.NewReader(raw))
	if err == nil && len(parsed.AST.Children) == 1 {
		_, err = buildkitinstructions.ParseCommand(parsed.AST.Children[0])
	} else if err == nil {
		err = fmt.Errorf("expected exactly one instruction")
	}
	if err != nil {
		return "", fmt.Errorf("ONBUILD %s cannot be represented as Dockerfile metadata: %w", strings.ToUpper(inst.Name), err)
	}
	return raw, nil
}

func formatOnBuildInstruction(inst Instruction) (string, error) {
	keyword := strings.ToUpper(inst.Name)
	switch inst.Name {
	case "arg":
		if len(inst.Arguments) == 1 {
			return "ARG " + inst.Arguments[0], nil
		}
		if len(inst.Arguments) == 2 {
			value, err := quoteDockerWord(inst.Arguments[1])
			if err != nil {
				return "", err
			}
			return "ARG " + inst.Arguments[0] + "=" + value, nil
		}
	case "run":
		return formatOnBuildRun(inst)
	case "copy", "add":
		return formatOnBuildCopyOrAdd(inst)
	case "env", "label":
		values, err := formatKeyValues(inst)
		if err != nil {
			return "", err
		}
		return keyword + " " + strings.Join(values, " "), nil
	case "cmd", "entrypoint":
		if inst.Form == "shell" {
			return formatOnBuildShellLine(keyword, inst.Arguments)
		}
		encoded, err := marshalStringArray(inst.Arguments)
		if err != nil {
			return "", err
		}
		return keyword + " " + encoded, nil
	case "shell":
		if err := validateOnBuildProperties(inst); err != nil {
			return "", err
		}
		encoded, err := marshalStringArray(inst.Arguments)
		if err != nil {
			return "", err
		}
		return "SHELL " + encoded, nil
	case "workdir", "user", "stopsignal":
		if err := validateOnBuildProperties(inst); err != nil {
			return "", err
		}
		if len(inst.Arguments) == 1 {
			value, err := quoteDockerWord(inst.Arguments[0])
			if err != nil {
				return "", err
			}
			return keyword + " " + value, nil
		}
	case "expose":
		if err := validateOnBuildProperties(inst); err != nil {
			return "", err
		}
		return formatWordList(keyword, inst.Arguments)
	case "volume":
		if err := validateOnBuildProperties(inst); err != nil {
			return "", err
		}
		encoded, err := marshalDockerWordArray(inst.Arguments)
		if err != nil {
			return "", err
		}
		return "VOLUME " + encoded, nil
	case "healthcheck":
		return formatOnBuildHealthcheck(inst)
	default:
		return "", fmt.Errorf("ONBUILD %s is not supported", keyword)
	}
	return "", fmt.Errorf("ONBUILD %s has invalid arguments", keyword)
}

func formatOnBuildRun(inst Instruction) (string, error) {
	if err := validateOnBuildProperties(inst, "network", "security"); err != nil {
		return "", err
	}
	flags := make([]string, 0, len(inst.Children)+2)
	for _, name := range []string{"network", "security"} {
		if value, ok := inst.Properties[name]; ok && value != "" {
			if strings.Contains(value, "$") {
				return "", fmt.Errorf("ONBUILD RUN %s must be a literal value; Dockerfile ONBUILD does not expand this flag", name)
			}
			flags = append(flags, "--"+name+"="+value)
		}
	}
	for _, child := range inst.Children {
		switch child.Name {
		case "mount":
			mount, err := formatOnBuildMount(child)
			if err != nil {
				return "", err
			}
			flags = append(flags, "--mount="+mount)
		case "device":
			requiredValue, requiredSet := child.Properties["required"]
			requiredOverride := false
			if requiredSet {
				if strings.Contains(requiredValue, "$") {
					return "", fmt.Errorf("ONBUILD RUN device required must be a literal boolean; Dockerfile ONBUILD does not expand this flag")
				}
				var err error
				requiredOverride, err = strconv.ParseBool(requiredValue)
				if err != nil {
					return "", fmt.Errorf("ONBUILD RUN device: required must be true or false")
				}
			}
			for _, selector := range child.Arguments {
				device, err := buildkitinstructions.ParseDevice(selector)
				if err != nil {
					return "", fmt.Errorf("ONBUILD RUN device selector %q: %w", selector, err)
				}
				if strings.TrimSpace(device.Name) == "" {
					return "", fmt.Errorf("ONBUILD RUN device selector is empty")
				}
				if requiredSet {
					device.Required = requiredOverride
				}
				value := device.Name
				if device.Required {
					value += ",required"
				}
				flags = append(flags, "--device="+value)
			}
		default:
			return "", fmt.Errorf("ONBUILD RUN child %q is not supported", child.Name)
		}
	}
	prefix := "RUN"
	if len(flags) != 0 {
		prefix += " " + strings.Join(flags, " ")
	}
	if inst.Form == "exec" {
		if len(inst.InlineFiles) != 0 {
			return formatExecutableRunHeredoc(prefix, inst)
		}
		encoded, err := marshalStringArray(inst.Arguments)
		if err != nil {
			return "", err
		}
		return prefix + " " + encoded, nil
	}
	if inst.Form != "" && inst.Form != "shell" {
		return "", fmt.Errorf("ONBUILD RUN form %q is not supported", inst.Form)
	}
	if len(inst.Arguments) != 1 {
		return "", fmt.Errorf("ONBUILD RUN shell form requires one command")
	}
	command := inst.Arguments[0]
	if !strings.ContainsAny(command, "\r\n") {
		return prefix + " " + dockerShellCommand(command), nil
	}
	if strings.ContainsRune(command, '\r') {
		return "", fmt.Errorf("ONBUILD RUN multiline shell command must use LF lines")
	}
	delimiter := unusedHeredocDelimiter(command, "COOPR_EOF")
	return prefix + " <<'" + delimiter + "'\n" + strings.TrimSuffix(command, "\n") + "\n" + delimiter, nil
}

func formatExecutableRunHeredoc(prefix string, inst Instruction) (string, error) {
	if len(inst.Arguments) != 1 || len(inst.InlineFiles) != 1 {
		return "", fmt.Errorf("ONBUILD RUN inline executable requires one argument and one file")
	}
	file := inst.InlineFiles[0]
	want := "/run/coopr-heredoc/" + file.Path
	if inst.Arguments[0] != want || !strings.HasPrefix(file.Data, "#!") || strings.ContainsRune(file.Data, '\r') {
		return "", fmt.Errorf("ONBUILD RUN inline executable is not representable as a Docker heredoc")
	}
	if heredocContainsDelimiter(file.Data, file.Path) {
		return "", fmt.Errorf("ONBUILD RUN inline executable collides with heredoc delimiter %q", file.Path)
	}
	return prefix + " <<'" + file.Path + "'\n" + strings.TrimSuffix(file.Data, "\n") + "\n" + file.Path, nil
}

func formatOnBuildCopyOrAdd(inst Instruction) (string, error) {
	if len(inst.InlineFiles) != 0 {
		return "", fmt.Errorf("ONBUILD %s inline sources cannot be authored", strings.ToUpper(inst.Name))
	}
	allowed := []string{"chown", "chmod", "link", "parents"}
	if inst.Name == "copy" {
		allowed = append(allowed, "from")
	} else {
		allowed = append(allowed, "checksum", "keep-git-dir", "unpack")
	}
	if err := validateOnBuildProperties(inst, allowed...); err != nil {
		return "", err
	}
	flags := make([]string, 0, len(inst.Properties)+len(inst.Children))
	propertyOrder := []string{"from", "chown", "chmod", "link", "parents", "checksum", "keep-git-dir", "unpack"}
	for _, name := range propertyOrder {
		value, ok := inst.Properties[name]
		if !ok || value == "" {
			continue
		}
		switch name {
		case "link", "parents", "keep-git-dir", "unpack":
			if strings.Contains(value, "$") {
				return "", fmt.Errorf("ONBUILD %s %s must be a literal boolean; Dockerfile ONBUILD does not expand this flag", strings.ToUpper(inst.Name), name)
			}
		}
		flags = append(flags, "--"+name+"="+value)
	}
	for _, child := range inst.Children {
		if child.Name != "exclude" || len(child.Properties) != 0 || len(child.Children) != 0 {
			return "", fmt.Errorf("ONBUILD %s child %q is not supported", strings.ToUpper(inst.Name), child.Name)
		}
		for _, pattern := range child.Arguments {
			value, err := quoteDockerWord(pattern)
			if err != nil {
				return "", err
			}
			flags = append(flags, "--exclude="+value)
		}
	}
	encoded, err := marshalDockerWordArray(inst.Arguments)
	if err != nil {
		return "", err
	}
	result := strings.ToUpper(inst.Name)
	if len(flags) != 0 {
		result += " " + strings.Join(flags, " ")
	}
	return result + " " + encoded, nil
}

func formatOnBuildMount(inst Instruction) (string, error) {
	if len(inst.Arguments) != 1 {
		return "", fmt.Errorf("ONBUILD RUN mount requires one type")
	}
	// ONBUILD uses Dockerfile metadata, whose mount vocabulary is narrower
	// than native Buildah execution. Reject unrepresentable options now, even
	// when supported fields still contain child-stage expressions.
	if err := ValidateProperties(inst.Properties, "from", "source", "src", "target", "dst", "destination", "readonly", "ro", "readwrite", "rw", "required", "size", "id", "sharing", "mode", "uid", "gid", "env"); err != nil {
		return "", fmt.Errorf("ONBUILD RUN mount Dockerfile metadata: %w", err)
	}
	fields := []string{"type=" + inst.Arguments[0]}
	keys := make([]string, 0, len(inst.Properties))
	for key := range inst.Properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value, err := escapeDockerFlagValue(inst.Properties[key])
		if err != nil {
			return "", fmt.Errorf("ONBUILD RUN mount %s: %w", key, err)
		}
		fields = append(fields, key+"="+value)
	}
	var encoded strings.Builder
	writer := csv.NewWriter(&encoded)
	if err := writer.Write(fields); err != nil {
		return "", fmt.Errorf("encode ONBUILD RUN mount: %w", err)
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return "", fmt.Errorf("encode ONBUILD RUN mount: %w", err)
	}
	raw := strings.TrimSuffix(encoded.String(), "\n")
	if !strings.Contains(raw, "$") {
		parsed, err := buildkitparser.Parse(strings.NewReader("RUN --mount=" + raw + " true"))
		if err != nil {
			return "", fmt.Errorf("ONBUILD RUN mount: %w", err)
		}
		command, err := buildkitinstructions.ParseCommand(parsed.AST.Children[0])
		if err == nil {
			err = command.(*buildkitinstructions.RunCommand).Expand(func(value string) (string, error) { return value, nil })
		}
		if err != nil {
			return "", fmt.Errorf("ONBUILD RUN mount Dockerfile metadata: %w", err)
		}
	}
	return raw, nil
}

func formatOnBuildHealthcheck(inst Instruction) (string, error) {
	if len(inst.Arguments) == 0 {
		return "", fmt.Errorf("ONBUILD HEALTHCHECK has no mode")
	}
	if inst.Arguments[0] == "NONE" {
		return "HEALTHCHECK NONE", nil
	}
	flags := make([]string, 0, len(inst.Properties))
	for _, name := range []string{"interval", "timeout", "start-period", "start-interval", "retries"} {
		if value, ok := inst.Properties[name]; ok {
			if strings.Contains(value, "$") {
				return "", fmt.Errorf("ONBUILD HEALTHCHECK %s must be a literal value; Dockerfile ONBUILD does not expand this flag", name)
			}
			flags = append(flags, "--"+name+"="+value)
		}
	}
	prefix := "HEALTHCHECK"
	if len(flags) != 0 {
		prefix += " " + strings.Join(flags, " ")
	}
	switch inst.Arguments[0] {
	case "CMD":
		encoded, err := marshalStringArray(inst.Arguments[1:])
		if err != nil {
			return "", err
		}
		return prefix + " CMD " + encoded, nil
	case "CMD-SHELL":
		if len(inst.Arguments) != 2 || strings.ContainsAny(inst.Arguments[1], "\r\n") {
			return "", fmt.Errorf("ONBUILD HEALTHCHECK shell command must be one line")
		}
		return prefix + " CMD " + dockerShellCommand(inst.Arguments[1]), nil
	default:
		return "", fmt.Errorf("ONBUILD HEALTHCHECK mode %q is not supported", inst.Arguments[0])
	}
}

func formatOnBuildShellLine(keyword string, arguments []string) (string, error) {
	if len(arguments) != 1 || strings.ContainsAny(arguments[0], "\r\n") {
		return "", fmt.Errorf("ONBUILD %s shell form requires one single-line command", keyword)
	}
	return keyword + " " + dockerShellCommand(arguments[0]), nil
}

func formatWordList(keyword string, arguments []string) (string, error) {
	words := make([]string, len(arguments))
	for index, argument := range arguments {
		if strings.ContainsAny(argument, " \t\r\n\x00") {
			return "", fmt.Errorf("ONBUILD %s argument %q is not a single Dockerfile word", keyword, argument)
		}
		words[index] = argument
	}
	return keyword + " " + strings.Join(words, " "), nil
}

func formatKeyValues(inst Instruction) ([]string, error) {
	if len(inst.Arguments) == 2 {
		value, err := quoteDockerWord(inst.Arguments[1])
		if err != nil {
			return nil, err
		}
		return []string{inst.Arguments[0] + "=" + value}, nil
	}
	keys := make([]string, 0, len(inst.Properties))
	for key := range inst.Properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		value, err := quoteDockerWord(inst.Properties[key])
		if err != nil {
			return nil, err
		}
		result = append(result, key+"="+value)
	}
	return result, nil
}

func validateOnBuildProperties(inst Instruction, allowed ...string) error {
	if err := ValidateProperties(inst.Properties, allowed...); err != nil {
		return fmt.Errorf("ONBUILD %s: %w", strings.ToUpper(inst.Name), err)
	}
	return nil
}

func quoteDockerWord(value string) (string, error) {
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("dockerfile word cannot contain a newline or NUL")
	}
	if value == "" {
		return `""`, nil
	}
	var encoded strings.Builder
	escaped := false
	for _, character := range value {
		if escaped {
			encoded.WriteRune(character)
			escaped = false
			continue
		}
		if character == '\\' {
			encoded.WriteRune(character)
			escaped = true
			continue
		}
		if character == '\'' || character == '"' || character == ' ' || character == '\t' {
			encoded.WriteByte('\\')
		}
		encoded.WriteRune(character)
	}
	return encoded.String(), nil
}

func escapeDockerFlagValue(value string) (string, error) {
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("dockerfile flag value cannot contain a newline or NUL")
	}
	var escaped strings.Builder
	for _, character := range value {
		switch character {
		case '\\':
			escaped.WriteString(`\\\`)
		case ' ', '\t':
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(character)
	}
	return escaped.String(), nil
}

func marshalStringArray(arguments []string) (string, error) {
	if arguments == nil {
		arguments = []string{}
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return "", fmt.Errorf("encode Dockerfile argument array: %w", err)
	}
	return string(encoded), nil
}

func marshalDockerWordArray(arguments []string) (string, error) {
	encodedArguments := make([]string, len(arguments))
	for index, argument := range arguments {
		encoded, err := quoteDockerWord(argument)
		if err != nil {
			return "", err
		}
		encodedArguments[index] = encoded
	}
	return marshalStringArray(encodedArguments)
}

func dockerShellCommand(command string) string {
	trimmed := strings.TrimSpace(command)
	if !strings.HasPrefix(trimmed, "[") {
		return command
	}
	var values []string
	if json.Unmarshal([]byte(trimmed), &values) == nil {
		// An empty quoted word is a shell no-op but prevents Dockerfile JSON
		// detection from changing an explicitly shell-form command to exec.
		return "''" + command
	}
	return command
}

func unusedHeredocDelimiter(contents, base string) string {
	for suffix := 0; ; suffix++ {
		candidate := base
		if suffix != 0 {
			candidate += "_" + strconv.Itoa(suffix)
		}
		if !heredocContainsDelimiter(contents, candidate) {
			return candidate
		}
	}
}

func heredocContainsDelimiter(contents, delimiter string) bool {
	for _, line := range strings.Split(contents, "\n") {
		if line == delimiter {
			return true
		}
	}
	return false
}
