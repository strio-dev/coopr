// Package onbuildparse converts Docker-format ONBUILD trigger strings into
// Coopr's executor-neutral instruction model.
package onbuildparse

import (
	"encoding/csv"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"coopr/internal/definition"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
)

// Expander expands one Dockerfile word while parsing a trigger.
type Expander func(string) (string, error)

// Parse parses a trigger without expanding child-stage variables.
func Parse(raw string) ([]definition.Instruction, error) {
	result, err := parseDeferred(raw, false)
	if err != nil {
		return nil, err
	}
	lex := shell.NewLex('\\')
	lex.SkipUnsetEnv = true
	for i := range result {
		if err := processDeferredWords(&result[i], func(word string) (string, error) {
			processed, _, err := lex.ProcessWord(word, shell.EnvsFromSlice(nil))
			return processed, err
		}); err != nil {
			return nil, fmt.Errorf("normalize ONBUILD instruction: %w", err)
		}
	}
	return result, nil
}

// ParseDeferred parses a trigger for later planner expansion. Dockerfile words
// retain their lexical quoting until the child-stage scope is available. COPY
// and ADD heredoc contents remain raw for the planner's heredoc expansion pass.
func ParseDeferred(raw string) ([]definition.Instruction, error) {
	return parseDeferred(raw, true)
}

func parseDeferred(raw string, deferMounts bool) ([]definition.Instruction, error) {
	identity := func(word string) (string, error) { return word, nil }
	result, err := parse(raw, identity, identity, true, deferMounts)
	if err != nil {
		return nil, err
	}
	for i := range result {
		markDeferredWords(&result[i])
	}
	return result, nil
}

func markDeferredWords(inst *definition.Instruction) {
	switch inst.Name {
	case "run", "cmd", "entrypoint", "healthcheck", "shell":
		// Runtime payloads and JSON shell vectors are already in their final
		// representation. RUN children are marked independently below.
		inst.ProcessQuotes = false
	default:
		inst.ProcessQuotes = true
	}
	for i := range inst.Children {
		markDeferredWords(&inst.Children[i])
	}
}

func processDeferredWords(inst *definition.Instruction, process Expander) error {
	if inst.ProcessQuotes {
		if inst.Name != "run" && inst.Name != "cmd" && inst.Name != "entrypoint" && inst.Name != "healthcheck" {
			for i := range inst.Arguments {
				value, err := process(inst.Arguments[i])
				if err != nil {
					return err
				}
				inst.Arguments[i] = value
			}
		}
		for name, authored := range inst.Properties {
			value, err := process(authored)
			if err != nil {
				return err
			}
			inst.Properties[name] = value
		}
		inst.ProcessQuotes = false
	}
	for i := range inst.Children {
		if err := processDeferredWords(&inst.Children[i], process); err != nil {
			return err
		}
	}
	return nil
}

// ParseExpanded parses a trigger and expands fields that Docker expands before
// execution. ARG defaults remain unexpanded for child-stage planning.
func ParseExpanded(raw string, expand Expander) ([]definition.Instruction, error) {
	return ParseExpandedRaw(raw, expand, expand)
}

// ParseExpandedRaw parses a trigger with separate normal and raw expanders.
// Docker uses raw expansion for unquoted COPY and ADD heredoc contents so
// quotes and other shell syntax in the generated file remain intact.
func ParseExpandedRaw(raw string, expand, expandRaw Expander) ([]definition.Instruction, error) {
	return parse(raw, expand, expandRaw, false, false)
}

func parse(raw string, expand, expandRaw Expander, retainExpansion, deferMounts bool) ([]definition.Instruction, error) {
	if expand == nil || expandRaw == nil {
		return nil, fmt.Errorf("ONBUILD expander is nil")
	}
	if strings.TrimSpace(raw) == "" || strings.ContainsRune(raw, '\x00') {
		return nil, fmt.Errorf("ONBUILD trigger must be one nonempty Dockerfile instruction without NUL")
	}
	parsed, err := parser.Parse(strings.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse ONBUILD trigger: %w", err)
	}
	if len(parsed.AST.Children) != 1 {
		return nil, fmt.Errorf("ONBUILD trigger must contain exactly one instruction")
	}
	command, err := instructions.ParseCommand(parsed.AST.Children[0])
	if err != nil {
		return nil, fmt.Errorf("parse ONBUILD instruction: %w", err)
	}
	if _, isArg := command.(*instructions.ArgCommand); !isArg {
		_, isRun := command.(*instructions.RunCommand)
		if expandable, ok := command.(instructions.SupportsSingleWordExpansion); ok && (!deferMounts || !isRun) {
			if err := expandable.Expand(instructions.SingleWordExpander(expand)); err != nil {
				return nil, fmt.Errorf("expand ONBUILD instruction: %w", err)
			}
		}
	}
	if expandable, ok := command.(instructions.SupportsSingleWordExpansionRaw); ok {
		if err := expandable.ExpandRaw(instructions.SingleWordExpander(expandRaw)); err != nil {
			return nil, fmt.Errorf("expand raw ONBUILD instruction: %w", err)
		}
	}
	var result []definition.Instruction
	switch value := command.(type) {
	case *instructions.ArgCommand:
		for _, pair := range value.Args {
			arguments := []string{pair.Key}
			if pair.Value != nil {
				arguments = append(arguments, *pair.Value)
			}
			result = append(result, definition.Instruction{Name: "arg", Arguments: arguments})
		}
	case *instructions.RunCommand:
		security := instructions.GetSecurity(value)
		if security != instructions.SecuritySandbox && security != instructions.SecurityInsecure {
			return nil, fmt.Errorf("ONBUILD RUN security mode %q is not supported", security)
		}
		inst, err := runInstruction(value)
		if err != nil {
			return nil, err
		}
		if security == instructions.SecurityInsecure {
			putProperty(&inst, "security", security)
		}
		if network := instructions.GetNetwork(value); network != instructions.NetworkDefault {
			putProperty(&inst, "network", network)
		}
		if deferMounts {
			mounts, err := deferredRunMounts(parsed.AST.Children[0].Flags)
			if err != nil {
				return nil, err
			}
			inst.Children = append(inst.Children, mounts...)
			if len(mounts) != 0 {
				inst.DeferredOnBuild = raw
			}
		} else {
			for _, mount := range instructions.GetMounts(value) {
				child, err := runMount(mount)
				if err != nil {
					return nil, err
				}
				inst.Children = append(inst.Children, child)
			}
		}
		for _, device := range instructions.GetDevices(value) {
			if device.Name == "" {
				return nil, fmt.Errorf("ONBUILD RUN device selector is empty")
			}
			child := definition.Instruction{Name: "device", Arguments: []string{device.Name}}
			if device.Required {
				putProperty(&child, "required", "true")
			}
			inst.Children = append(inst.Children, child)
		}
		result = []definition.Instruction{inst}
	case *instructions.CopyCommand:
		inst, err := copyOrAddInstruction("copy", value.SourcePaths, value.SourceContents, value.DestPath, retainExpansion)
		if err != nil {
			return nil, err
		}
		putProperty(&inst, "from", value.From)
		putProperty(&inst, "chown", value.Chown)
		putProperty(&inst, "chmod", value.Chmod)
		if value.Link {
			putProperty(&inst, "link", "true")
		}
		if value.Parents {
			putProperty(&inst, "parents", "true")
		}
		addExcludes(&inst, value.ExcludePatterns)
		result = []definition.Instruction{inst}
	case *instructions.AddCommand:
		inst, err := copyOrAddInstruction("add", value.SourcePaths, value.SourceContents, value.DestPath, retainExpansion)
		if err != nil {
			return nil, err
		}
		putProperty(&inst, "chown", value.Chown)
		putProperty(&inst, "chmod", value.Chmod)
		putProperty(&inst, "checksum", value.Checksum)
		if value.KeepGitDir != nil {
			putProperty(&inst, "keep-git-dir", strconv.FormatBool(*value.KeepGitDir))
		}
		if value.Unpack != nil {
			putProperty(&inst, "unpack", strconv.FormatBool(*value.Unpack))
		}
		if value.Link {
			putProperty(&inst, "link", "true")
		}
		addExcludes(&inst, value.ExcludePatterns)
		result = []definition.Instruction{inst}
	case *instructions.EnvCommand:
		if len(value.Env) == 1 {
			pair := value.Env[0]
			result = []definition.Instruction{{Name: "env", Arguments: []string{pair.Key, pair.Value}}}
			break
		}
		properties := make(map[string]string, len(value.Env))
		for _, pair := range value.Env {
			properties[pair.Key] = pair.Value
		}
		result = []definition.Instruction{{Name: "env", Properties: properties}}
	case *instructions.LabelCommand:
		for _, pair := range value.Labels {
			result = append(result, definition.Instruction{Name: "label", Arguments: []string{pair.Key, pair.Value}})
		}
	case *instructions.WorkdirCommand:
		result = []definition.Instruction{{Name: "workdir", Arguments: []string{value.Path}}}
	case *instructions.UserCommand:
		result = []definition.Instruction{{Name: "user", Arguments: []string{value.User}}}
	case *instructions.CmdCommand:
		result = []definition.Instruction{shellDependentInstruction("cmd", value.ShellDependantCmdLine)}
	case *instructions.EntrypointCommand:
		result = []definition.Instruction{shellDependentInstruction("entrypoint", value.ShellDependantCmdLine)}
	case *instructions.ShellCommand:
		result = []definition.Instruction{{Name: "shell", Arguments: slices.Clone(value.Shell)}}
	case *instructions.ExposeCommand:
		for _, port := range value.Ports {
			result = append(result, definition.Instruction{Name: "expose", Arguments: []string{port}})
		}
	case *instructions.VolumeCommand:
		for _, volume := range value.Volumes {
			result = append(result, definition.Instruction{Name: "volume", Arguments: []string{volume}})
		}
	case *instructions.StopSignalCommand:
		result = []definition.Instruction{{Name: "stopsignal", Arguments: []string{value.Signal}}}
	case *instructions.HealthCheckCommand:
		if value.Health == nil {
			return nil, fmt.Errorf("ONBUILD HEALTHCHECK has no configuration")
		}
		inst := definition.Instruction{Name: "healthcheck", Arguments: slices.Clone(value.Health.Test)}
		for _, property := range []struct {
			name     string
			duration time.Duration
		}{
			{"interval", value.Health.Interval}, {"timeout", value.Health.Timeout},
			{"start-period", value.Health.StartPeriod}, {"start-interval", value.Health.StartInterval},
		} {
			if property.duration != 0 {
				putProperty(&inst, property.name, property.duration.String())
			}
		}
		if value.Health.Retries != 0 {
			putProperty(&inst, "retries", strconv.Itoa(value.Health.Retries))
		}
		result = []definition.Instruction{inst}
	default:
		return nil, fmt.Errorf("ONBUILD %s is not supported", strings.ToUpper(command.Name()))
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("ONBUILD trigger has no executable instruction")
	}
	for _, inst := range result {
		var err error
		if retainExpansion {
			err = definition.Validate(&definition.Definition{Instructions: []definition.Instruction{inst}})
		} else {
			err = definition.ValidateResolved(inst)
		}
		if err != nil {
			return nil, fmt.Errorf("ONBUILD %s: %w", strings.ToUpper(inst.Name), err)
		}
	}
	return result, nil
}

func runInstruction(command *instructions.RunCommand) (definition.Instruction, error) {
	if len(command.Files) == 0 {
		return shellDependentInstruction("run", command.ShellDependantCmdLine), nil
	}
	if len(command.CmdLine) != 1 || !command.PrependShell {
		return definition.Instruction{}, fmt.Errorf("ONBUILD RUN heredoc produced an invalid command")
	}

	if parser.MustParseHeredoc(command.CmdLine[0]) != nil {
		if len(command.Files) != 1 {
			return definition.Instruction{}, fmt.Errorf("ONBUILD RUN simple heredoc must contain exactly one document")
		}
		contents := command.Files[0].Data
		if command.Files[0].Chomp {
			contents = parser.ChompHeredocContent(contents)
		}
		if strings.HasPrefix(contents, "#!") {
			name := command.Files[0].Name
			return definition.Instruction{
				Name: "run", Form: "exec", Arguments: []string{path.Join("/run/coopr-heredoc", name)},
				InlineFiles: []definition.InlineFile{{Path: name, Data: contents}},
			}, nil
		}
		return definition.Instruction{Name: "run", Form: "shell", Arguments: []string{contents}}, nil
	}

	var contents strings.Builder
	contents.WriteString(command.CmdLine[0])
	for _, file := range command.Files {
		contents.WriteByte('\n')
		contents.WriteString(file.Data)
		contents.WriteString(file.Name)
	}
	return definition.Instruction{Name: "run", Form: "shell", Arguments: []string{contents.String()}}, nil
}

func copyOrAddInstruction(name string, paths []string, contents []instructions.SourceContent, destination string, retainExpansion bool) (definition.Instruction, error) {
	inst := definition.Instruction{Name: name, Arguments: append(slices.Clone(paths), destination)}
	for _, content := range contents {
		inst.InlineFiles = append(inst.InlineFiles, definition.InlineFile{
			Path: content.Path, Data: content.Data, Expand: retainExpansion && content.Expand,
		})
	}
	return inst, nil
}

func shellDependentInstruction(name string, value instructions.ShellDependantCmdLine) definition.Instruction {
	inst := definition.Instruction{Name: name, Form: "exec", Arguments: slices.Clone(value.CmdLine)}
	if value.PrependShell {
		inst.Form = "shell"
	}
	return inst
}

func putProperty(inst *definition.Instruction, name, value string) {
	if value == "" {
		return
	}
	if inst.Properties == nil {
		inst.Properties = make(map[string]string)
	}
	inst.Properties[name] = value
}

func addExcludes(inst *definition.Instruction, patterns []string) {
	if len(patterns) != 0 {
		inst.Children = append(inst.Children, definition.Instruction{Name: "exclude", Arguments: slices.Clone(patterns)})
	}
}

// Retain unresolved RUN mount fields for graph dependency discovery. The original
// trigger is decoded by the upstream typed parser once child-stage scope exists;
// these provisional fields never supply execution or cache semantics.
func deferredRunMounts(flags []string) ([]definition.Instruction, error) {
	var mounts []definition.Instruction
	for _, flag := range flags {
		value, ok := strings.CutPrefix(flag, "--mount=")
		if !ok {
			continue
		}
		fields, err := csv.NewReader(strings.NewReader(value)).Read()
		if err != nil {
			return nil, fmt.Errorf("parse deferred ONBUILD RUN mount: %w", err)
		}
		mount := definition.Instruction{Name: "mount", Arguments: []string{"bind"}}
		for _, field := range fields {
			key, value, hasValue := strings.Cut(field, "=")
			key = strings.ToLower(key)
			if !hasValue {
				switch key {
				case "readonly", "ro", "readwrite", "rw", "required":
					value = "true"
				default:
					return nil, fmt.Errorf("ONBUILD RUN mount field %q requires a value", key)
				}
			}
			if key == "type" {
				if !strings.Contains(value, "$") {
					value = strings.ToLower(value)
				}
				mount.Arguments[0] = value
			} else {
				if key == "sharing" && !strings.Contains(value, "$") {
					value = strings.ToLower(value)
				}
				// Docker's aliases assign the same field in source order. Keep
				// the last assignment without evaluating boolean expressions.
				switch key {
				case "src":
					key = "source"
				case "dst", "destination":
					key = "target"
				case "ro", "readonly":
					key = "readonly"
					delete(mount.Properties, "rw")
				case "rw", "readwrite":
					key = "rw"
					delete(mount.Properties, "readonly")
				}
				if mount.Properties == nil {
					mount.Properties = make(map[string]string)
				}
				mount.Properties[key] = value
			}
		}
		mounts = append(mounts, mount)
	}
	return mounts, nil
}

func runMount(mount *instructions.Mount) (definition.Instruction, error) {
	if mount == nil {
		return definition.Instruction{}, fmt.Errorf("ONBUILD RUN has a nil mount")
	}
	child := definition.Instruction{Name: "mount", Arguments: []string{string(mount.Type)}}
	putProperty(&child, "from", mount.From)
	putProperty(&child, "source", mount.Source)
	putProperty(&child, "target", mount.Target)
	if mount.Type == instructions.MountTypeBind || mount.ReadOnly && mount.Type != instructions.MountTypeSecret && mount.Type != instructions.MountTypeSSH {
		putProperty(&child, "readonly", strconv.FormatBool(mount.ReadOnly))
	}
	putProperty(&child, "id", mount.CacheID)
	putProperty(&child, "sharing", string(mount.CacheSharing))
	if mount.Required {
		putProperty(&child, "required", "true")
	}
	if mount.Env != nil {
		putProperty(&child, "env", *mount.Env)
	}
	if mount.Mode != nil {
		putProperty(&child, "mode", strconv.FormatUint(*mount.Mode, 8))
	}
	if mount.UID != nil {
		putProperty(&child, "uid", strconv.FormatUint(*mount.UID, 10))
	}
	if mount.GID != nil {
		putProperty(&child, "gid", strconv.FormatUint(*mount.GID, 10))
	}
	if mount.SizeLimit != 0 {
		putProperty(&child, "size", strconv.FormatInt(mount.SizeLimit, 10))
	}
	return child, nil
}
