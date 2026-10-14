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
	docker "github.com/fsouza/go-dockerclient"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/openshift/imagebuilder"
)

// Expander expands one Dockerfile word while parsing a trigger.
type Expander func(string) (string, error)

// ValidateInheritedTriggers applies the native base-image ingestion policy.
// Generic instruction parsing also serves authored stages and must not impose
// ONBUILD-only restrictions on those instructions.
func ValidateInheritedTriggers(triggers []string) error {
	node, err := imagebuilder.ParseDockerfile(strings.NewReader("FROM scratch\n"))
	if err != nil {
		return err
	}
	return imagebuilder.NewBuilder(nil).FromImage(&docker.Image{Config: &docker.Config{OnBuild: triggers}}, node)
}

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
	result, err := parse(raw, identity, identity, identity, true, deferMounts, nil)
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
	case "run", "cmd", "entrypoint", "healthcheck", "shell", "maintainer":
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
	return ParseExpandedRawWithFlags(raw, expand, expandRaw, expand)
}

// ParseExpandedRawWithFlags separates structural flag expansion from ordinary
// source words. COPY --from can see global arguments without leaking those
// undeclared globals into sources, heredocs, or other options.
func ParseExpandedRawWithFlags(raw string, expand, expandRaw, expandFlag Expander) ([]definition.Instruction, error) {
	return parse(raw, expand, expandRaw, expandFlag, false, false, nil)
}

// ParseExpandedEnvironment normalizes imported triggers against one native
// environment snapshot, retaining EXPOSE word-list expansion.
func ParseExpandedEnvironment(raw string, environment []string) ([]definition.Instruction, error) {
	scalar := func(word string) (string, error) { return imagebuilder.ProcessWord(word, environment) }
	rawLex := shell.NewLex('\\')
	rawLex.SkipProcessQuotes = true
	rawWord := func(word string) (string, error) {
		result, _, err := rawLex.ProcessWord(word, shell.EnvsFromSlice(environment))
		return result, err
	}
	return parse(raw, scalar, rawWord, scalar, false, false, func(word string) ([]string, error) { return imagebuilder.ProcessWords(word, environment) })
}

func parse(raw string, expand, expandRaw, expandFlag Expander, retainExpansion, deferMounts bool, expandWords func(string) ([]string, error)) ([]definition.Instruction, error) {
	if expand == nil || expandRaw == nil || expandFlag == nil {
		return nil, fmt.Errorf("ONBUILD expander is nil")
	}
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("ONBUILD trigger must be one nonempty Dockerfile instruction")
	}
	if strings.ContainsRune(raw, '\x00') {
		if err := definition.ValidateOperation(definition.Instruction{Name: "onbuild", Arguments: []string{raw}}); err != nil {
			return nil, err
		}
	}
	parsed, err := parser.Parse(strings.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse ONBUILD trigger: %w", err)
	}
	if len(parsed.AST.Children) != 1 {
		return nil, fmt.Errorf("ONBUILD trigger must contain exactly one instruction")
	}
	node := parsed.AST.Children[0]
	name := strings.ToLower(node.Value)
	// BuildKit's typed RUN parser rejects Buildah's native mount vocabulary and
	// validates network before child ARG expansion. Keep its command/heredoc
	// decoding, with native flags translated separately into our existing schema.
	var runFlags []string
	var deferredProperties map[string]string
	var nativeCopyFlags []string
	var deferredCopy bool
	switch name {
	case "run":
		var typedFlags []string
		for _, flag := range node.Flags {
			if strings.HasPrefix(flag, "--mount=") || strings.HasPrefix(flag, "--network=") {
				if !retainExpansion {
					flag, err = expandFlag(flag)
					if err != nil {
						return nil, fmt.Errorf("expand ONBUILD RUN flag: %w", err)
					}
				}
				runFlags = append(runFlags, flag)
			} else {
				typedFlags = append(typedFlags, flag)
			}
		}
		node.Flags = typedFlags
	case "copy", "add":
		deferredProperties = make(map[string]string)
		var typedFlags []string
		for _, flag := range node.Flags {
			if !retainExpansion {
				flag, err = expandFlag(flag)
				if err != nil {
					return nil, fmt.Errorf("expand ONBUILD flag: %w", err)
				}
			}
			key, value, hasValue := strings.Cut(strings.TrimPrefix(flag, "--"), "=")
			if retainExpansion && hasValue && strings.Contains(value, "$") {
				deferredCopy = true
				switch key {
				case "link", "parents", "keep-git-dir", "unpack", "chmod":
					deferredProperties[key] = value
					placeholder := "false"
					if key == "chmod" {
						placeholder = "0644"
					}
					flag = "--" + key + "=" + placeholder
				}
			}
			if !strings.Contains(value, "$") {
				delete(deferredProperties, key)
			}
			if name == "add" && key == "unpack" {
				// The pinned native Copy API has no unpack field. Keep this
				// existing option on its typed decoding path.
				typedFlags = append(typedFlags, flag)
				continue
			}
			if retainExpansion {
				concrete := !strings.Contains(value, "$")
				switch key {
				case "link", "parents", "keep-git-dir", "chmod":
					concrete = true // Native validation uses the deferred placeholder.
				}
				if concrete {
					// The text parser removes outer delimiters, but nested quotes
					// and escapes still need native processing before validation.
					flag, err = imagebuilder.ProcessWord(flag, nil)
					if err != nil {
						return nil, err
					}
				}
			}
			// Expansion happens at our child-scope boundary, once. Quote the
			// result for the native dispatcher's own ProcessWord call.
			nativeCopyFlags = append(nativeCopyFlags, "'"+strings.ReplaceAll(flag, "'", "'\"'\"'")+"'")
		}
		node.Flags = typedFlags
	}
	var command instructions.Command
	if name == "healthcheck" {
		// imagebuilder permits any positive duration, including sub-millisecond
		// checks; BuildKit's typed parser imposes an additional minimum.
		native, parseErr := imagebuilder.ParseDockerfile(strings.NewReader(raw))
		if parseErr != nil {
			return nil, parseErr
		}
		builder := imagebuilder.NewBuilder(nil)
		step := builder.Step()
		if err = step.Resolve(native.Children[0]); err == nil {
			err = builder.Run(step, imagebuilder.NoopExecutor, true)
		}
		if err == nil {
			config := builder.RunConfig.Healthcheck
			command = &instructions.HealthCheckCommand{Health: &dockerspec.HealthcheckConfig{
				Test: config.Test, Interval: config.Interval, Timeout: config.Timeout,
				StartPeriod: config.StartPeriod, StartInterval: config.StartInterval, Retries: config.Retries,
			}}
		}
	} else {
		command, err = instructions.ParseCommand(node)
	}
	if err != nil {
		return nil, fmt.Errorf("parse ONBUILD instruction: %w", err)
	}
	if name == "copy" || name == "add" {
		copy, err := decodeNativeCopyFlags(name, nativeCopyFlags)
		if err != nil {
			return nil, fmt.Errorf("parse ONBUILD instruction: %w", err)
		}
		if retainExpansion && !deferredCopy {
			// These native results are already decoded. Protect literal quote/
			// escape bytes from the later source-word normalization pass.
			copy.From = deferredLiteral(copy.From)
			copy.Chown = deferredLiteral(copy.Chown)
			copy.Checksum = deferredLiteral(copy.Checksum)
			for i := range copy.Excludes {
				copy.Excludes[i] = deferredLiteral(copy.Excludes[i])
			}
		}
		switch value := command.(type) {
		case *instructions.CopyCommand:
			value.From, value.Chown, value.Chmod = copy.From, copy.Chown, copy.Chmod
			value.Link, value.Parents, value.ExcludePatterns = copy.Link, copy.Parents, copy.Excludes
		case *instructions.AddCommand:
			value.Chown, value.Chmod, value.Checksum = copy.Chown, copy.Chmod, copy.Checksum
			value.Link, value.ExcludePatterns = copy.Link, copy.Excludes
			if copy.KeepGitDir {
				keep := true
				value.KeepGitDir = &keep
			}
		}
	}
	if _, isArg := command.(*instructions.ArgCommand); !isArg {
		_, isRun := command.(*instructions.RunCommand)
		var expandCommand func(instructions.SingleWordExpander) error
		switch value := command.(type) {
		case *instructions.CopyCommand:
			expandCommand = value.SourcesAndDest.Expand
		case *instructions.AddCommand:
			expandCommand = value.SourcesAndDest.Expand
		default:
			if expandable, ok := command.(instructions.SupportsSingleWordExpansion); ok {
				expandCommand = expandable.Expand
			}
		}
		if expandCommand != nil && (!deferMounts || !isRun) {
			if err := expandCommand(instructions.SingleWordExpander(expand)); err != nil {
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
		for index, pair := range value.Args {
			arguments := []string{pair.Key}
			if pair.Value != nil {
				arguments = append(arguments, *pair.Value)
			}
			result = append(result, definition.Instruction{Name: "arg", Arguments: arguments, NativeContinuation: index > 0})
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
		for _, flag := range runFlags {
			if network, ok := strings.CutPrefix(flag, "--network="); ok {
				putProperty(&inst, "network", network)
				if network == "" {
					return nil, fmt.Errorf("ONBUILD RUN network requires a value")
				}
			}
		}
		mounts, err := deferredRunMounts(runFlags, !retainExpansion)
		if err != nil {
			return nil, err
		}
		if !deferMounts {
			mounts, err = resolvedRunMounts(runFlags, mounts)
			if err != nil {
				return nil, err
			}
		}
		inst.Children = append(inst.Children, mounts...)
		if deferMounts && (len(mounts) != 0 || strings.Contains(inst.Properties["network"], "$")) {
			inst.DeferredOnBuild = raw
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
		for index, pair := range value.Env {
			result = append(result, definition.Instruction{Name: "env", Arguments: []string{pair.Key, pair.Value}, NativeContinuation: index > 0})
		}
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
	case *instructions.MaintainerCommand:
		result = []definition.Instruction{{Name: "maintainer", Arguments: []string{value.Maintainer}}}
	case *instructions.ShellCommand:
		result = []definition.Instruction{{Name: "shell", Arguments: slices.Clone(value.Shell)}}
	case *instructions.ExposeCommand:
		inst := definition.Instruction{Name: "expose"}
		for word := node.Next; word != nil; word = word.Next {
			port := word.Value
			if expandWords != nil {
				words, err := expandWords(port)
				if err != nil {
					return nil, err
				}
				inst.Arguments = append(inst.Arguments, words...)
				continue
			}
			if !retainExpansion {
				port, err = expand(port)
				if err != nil {
					return nil, err
				}
			}
			inst.Arguments = append(inst.Arguments, port)
		}
		result = []definition.Instruction{inst}
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
	for i := range result {
		if !retainExpansion {
			result[i].NativeContinuation = false
		}
		if deferredCopy {
			result[i].DeferredOnBuild = raw
		}
		for key, value := range deferredProperties {
			putProperty(&result[i], key, value)
		}
	}
	for _, inst := range result {
		var err error
		if retainExpansion {
			err = definition.ValidateOperation(inst)
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
// trigger is decoded again once child-stage scope exists;
// these provisional fields never supply execution or cache semantics.
func deferredRunMounts(flags []string, resolved bool) ([]definition.Instruction, error) {
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
		relabelSeen := false
		for _, field := range fields {
			key, value, hasValue := strings.Cut(field, "=")
			// Buildah's native no-value options must be validated before their
			// representation is normalized or overwritten by a later alias.
			switch key {
			case "readonly", "ro", "readwrite", "rw", "bind-nonrecursive", "nosuid", "nodev", "noexec", "U", "u", "z", "Z", "shared", "rshared", "private", "rprivate", "slave", "rslave", "no-dereference", "tmpcopyup":
				if hasValue {
					return nil, fmt.Errorf("ONBUILD RUN mount %s does not accept a value", key)
				}
			case "sharing", "relabel", "bind-propagation":
				if !hasValue || value == "" {
					return nil, fmt.Errorf("ONBUILD RUN mount %s requires a nonempty value", key)
				}
				if key == "relabel" {
					if relabelSeen {
						return nil, fmt.Errorf("ONBUILD RUN mount relabel may only be specified once")
					}
					relabelSeen = true
				}
				if key == "sharing" && !strings.Contains(value, "$") {
					value = strings.ToLower(value)
				}
				check := definition.Instruction{Name: "run", Arguments: []string{"true"}, Children: []definition.Instruction{{Name: "mount", Arguments: []string{"cache"}, Properties: map[string]string{key: value}}}}
				validate := definition.ValidateOperation
				if resolved {
					validate = definition.ValidateResolved
				}
				if err := validate(check); err != nil {
					return nil, err
				}
			}
			switch key {
			case "Z":
				key, value, hasValue = "relabel", "private", true
			case "z":
				key, value, hasValue = "relabel", "shared", true
			default:
				key = strings.ToLower(key)
			}
			if !hasValue {
				switch key {
				case "readonly", "ro", "readwrite", "rw", "required", "bind-nonrecursive", "nosuid", "nodev", "noexec", "u", "z", "shared", "rshared", "private", "rprivate", "slave", "rslave", "no-dereference", "tmpcopyup":
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
				case "tmpfs-mode":
					key = "mode"
				case "tmpfs-size":
					key = "size"
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
		kind := mount.Arguments[0]
		if !strings.Contains(kind, "$") {
			for _, field := range fields {
				key, _, _ := strings.Cut(field, "=")
				if (kind == "tmpfs" && (key == "mode" || key == "size" || key == "rw" || key == "readwrite")) ||
					(kind != "tmpfs" && (key == "tmpfs-mode" || key == "tmpfs-size")) ||
					(kind == "cache" && key == "relabel") {
					return nil, fmt.Errorf("ONBUILD RUN %s mount does not support native field %s", kind, key)
				}
			}
		}
		if mount.Arguments[0] == "secret" || mount.Arguments[0] == "ssh" {
			for _, name := range []string{"readonly", "rw", "source"} {
				if _, exists := mount.Properties[name]; exists {
					return nil, fmt.Errorf("ONBUILD RUN %s mount does not support %s", mount.Arguments[0], name)
				}
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

// The typed decoder validates numeric and boolean fields in source order,
// including invalid values later overwritten by aliases. Native-only fields
// stay in the executor-neutral schema; Buildah's full mount parser is internal.
func resolvedRunMounts(flags []string, mounts []definition.Instruction) ([]definition.Instruction, error) {
	index := 0
	for _, flag := range flags {
		raw, ok := strings.CutPrefix(flag, "--mount=")
		if !ok {
			continue
		}
		fields, err := csv.NewReader(strings.NewReader(raw)).Read()
		if err != nil {
			return nil, err
		}
		original := mounts[index]
		var typedFields []string
		nativeFields := make(map[string]string)
		for _, field := range fields {
			key, _, _ := strings.Cut(field, "=")
			key = strings.ToLower(key)
			typed := false
			switch key {
			case "type", "from", "source", "src", "target", "dst", "destination", "readonly", "ro", "readwrite", "rw", "required", "size", "id", "sharing", "mode", "uid", "gid", "env":
				typed = true
			}
			if original.Arguments[0] == "tmpfs" && (key == "mode" || key == "size") {
				typed = false
			}
			if original.Arguments[0] == "secret" && (key == "source" || key == "src") {
				typed = false
			}
			if (original.Arguments[0] == "secret" || original.Arguments[0] == "ssh") && (key == "readonly" || key == "ro" || key == "readwrite" || key == "rw") {
				typed = false
			}
			if typed {
				typedFields = append(typedFields, field)
			} else {
				switch key {
				case "z":
					key = "relabel"
				case "src":
					key = "source"
				case "tmpfs-mode":
					key = "mode"
				case "tmpfs-size":
					key = "size"
				case "ro":
					key = "readonly"
				case "readwrite":
					key = "rw"
				}
				nativeFields[key] = original.Properties[key]
			}
		}
		var encoded strings.Builder
		writer := csv.NewWriter(&encoded)
		if err := writer.Write(typedFields); err != nil {
			return nil, err
		}
		writer.Flush()
		if err := writer.Error(); err != nil {
			return nil, err
		}
		node := &parser.Node{Value: "run", Flags: []string{"--mount=" + strings.TrimSuffix(encoded.String(), "\n")}, Next: &parser.Node{Value: "true"}, Original: "RUN true"}
		command, err := instructions.ParseCommand(node)
		if err != nil {
			return nil, err
		}
		run := command.(*instructions.RunCommand)
		if err := run.Expand(func(word string) (string, error) { return word, nil }); err != nil {
			return nil, fmt.Errorf("ONBUILD RUN mount: %w", err)
		}
		mount, err := runMount(instructions.GetMounts(run)[0])
		if err != nil {
			return nil, err
		}
		for key, value := range nativeFields {
			putProperty(&mount, key, value)
		}
		mounts[index] = mount
		index++
	}
	return mounts, nil
}

// nativeCopyCapture adapts the public native frontend result without executing
// a filesystem operation. Embedding the no-op executor retains native lifecycle
// callbacks while COPY only collects the dispatcher's validated representation.
type nativeCopyCapture struct {
	imagebuilder.Executor
	copies []imagebuilder.Copy
}

func (e *nativeCopyCapture) Copy(_ []string, copies ...imagebuilder.Copy) error {
	e.copies = append(e.copies, copies...)
	return nil
}

func decodeNativeCopyFlags(name string, flags []string) (imagebuilder.Copy, error) {
	builder := imagebuilder.NewBuilder(nil)
	capture := &nativeCopyCapture{Executor: imagebuilder.NoopExecutor}
	// Source/heredoc decoding remains on the existing typed parser. Dummy paths
	// isolate flag semantics from native destination absolutization and heredoc
	// expansion, both of which belong to later Coopr planning/execution.
	step := &imagebuilder.Step{Command: name, Args: []string{"source", "/destination"}, Flags: flags}
	if err := builder.Run(step, capture, true); err != nil {
		return imagebuilder.Copy{}, err
	}
	if len(capture.copies) != 1 {
		return imagebuilder.Copy{}, fmt.Errorf("native %s produced %d copies", name, len(capture.copies))
	}
	return capture.copies[0], nil
}

// WorkingDirectory delegates normalized WORKDIR metadata to the native frontend.
// Absolute paths retain native spelling; relative paths use the current directory.
func WorkingDirectory(current, requested string) (string, error) {
	builder := imagebuilder.NewBuilder(nil)
	builder.RunConfig.WorkingDir = current
	if err := builder.Run(&imagebuilder.Step{Command: "workdir", Args: []string{requested}}, imagebuilder.NoopExecutor, true); err != nil {
		return "", err
	}
	return builder.RunConfig.WorkingDir, nil
}

// ExposedPorts delegates EXPOSE token interpretation to the native
// frontend, including literal quoted tokens and default protocol handling.
func ExposedPorts(arguments []string) ([]string, error) {
	builder := imagebuilder.NewBuilder(nil)
	if err := builder.Run(&imagebuilder.Step{Command: "expose", Args: arguments}, imagebuilder.NoopExecutor, true); err != nil {
		return nil, err
	}
	ports := make([]string, 0, len(builder.RunConfig.ExposedPorts))
	for port := range builder.RunConfig.ExposedPorts {
		ports = append(ports, string(port))
	}
	slices.Sort(ports)
	return ports, nil
}

// deferredLiteral encodes an already processed native flag value for the
// existing one-time deferred Docker word normalization boundary.
func deferredLiteral(value string) string {
	if strings.ContainsAny(value, "\"'\\$") {
		return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
	}
	return value
}
