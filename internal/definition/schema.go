package definition

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// InstructionProperties lists the fixed option names for an instruction.
// ENV, LABEL and component arguments deliberately have user-defined keys.
func InstructionProperties(name string) ([]string, bool) {
	switch name {
	case "from":
		return []string{"as", "platform", "after"}, true
	case "extend":
		return []string{"as", "distro", "distro-version", "package-manager", "architecture"}, true
	case "package":
		return []string{"as"}, true
	case "run":
		return []string{"network", "security"}, true
	case "copy":
		return []string{"from", "chown", "chmod", "link", "parents"}, true
	case "add":
		return []string{"from", "chown", "chmod", "link", "parents", "checksum", "keep-git-dir", "unpack"}, true
	case "healthcheck":
		return []string{"interval", "timeout", "start-period", "start-interval", "retries"}, true
	case "device":
		return []string{"required"}, true
	case "env", "label", "component", "mount":
		return nil, false
	default:
		return nil, true
	}
}

// MountProperties lists canonical property names supported by an executor mount.
// Authored aliases are validated separately and normalized by the executor.
func MountProperties(kind string) ([]string, bool) {
	switch kind {
	case "bind":
		return []string{"source", "target", "readonly", "bind-nonrecursive", "nosuid", "nodev", "noexec", "shared", "rshared", "private", "rprivate", "slave", "rslave", "u", "no-dereference", "bind-propagation", "relabel", "consistency"}, true
	case "cache":
		return []string{"id", "source", "target", "readonly", "sharing", "mode", "uid", "gid", "nosuid", "nodev", "noexec", "u", "shared", "rshared", "private", "rprivate", "slave", "rslave", "bind-propagation", "relabel"}, true
	case "tmpfs":
		return []string{"target", "readonly", "size", "mode", "nosuid", "nodev", "noexec", "tmpcopyup"}, true
	case "secret":
		return []string{"id", "target", "required", "mode", "uid", "gid", "env"}, true
	case "ssh":
		return []string{"id", "target", "required", "mode", "uid", "gid"}, true
	default:
		return nil, false
	}
}

// ValidateProperties rejects unknown keys in a stable order and identifies the
// offending key together with the available options.
func ValidateProperties(properties map[string]string, allowed ...string) error {
	for _, key := range slices.Sorted(maps.Keys(properties)) {
		if !slices.Contains(allowed, key) {
			if len(allowed) == 0 {
				return fmt.Errorf("unsupported property %q; no properties are allowed", key)
			}
			names := slices.Clone(allowed)
			slices.Sort(names)
			names = slices.Compact(names)
			return fmt.Errorf("unsupported property %q; allowed properties: %s", key, strings.Join(names, ", "))
		}
	}
	return nil
}

func validateInstructionName(name string) error {
	if name != strings.ToLower(name) {
		return fmt.Errorf("invalid instruction name %q; use %q (instruction names are lowercase)", name, strings.ToLower(name))
	}
	switch name {
	case "layer", "exec", "from", "extend", "package", "arg", "run", "copy", "add", "env", "label", "workdir", "user", "cmd", "entrypoint", "healthcheck", "onbuild", "component", "mount", "device", "exclude", "shell", "stopsignal", "expose", "volume", "maintainer":
		return nil
	default:
		return fmt.Errorf("unknown instruction %q", name)
	}
}

func validateMountProperties(kind string, properties map[string]string, resolved bool) error {
	canonical, known := MountProperties(kind)
	if !known && (resolved || !strings.Contains(kind, "$")) {
		return fmt.Errorf("mount type %q is not supported", kind)
	}
	// An unresolved type may choose any supported mount. Its option names still
	// need to belong to at least one supported mount kind.
	if !known {
		for _, candidate := range []string{"bind", "cache", "tmpfs", "secret", "ssh"} {
			names, _ := MountProperties(candidate)
			canonical = append(canonical, names...)
		}
	}
	allowed := slices.Clone(canonical)
	aliases := map[string]string{"src": "source", "dst": "target", "destination": "target", "ro": "readonly", "rw": "readonly", "readwrite": "readonly", "tmpfs-mode": "mode", "tmpfs-size": "size", "U": "u", "z": "relabel", "Z": "relabel"}
	for alias, name := range aliases {
		if slices.Contains(canonical, name) || (kind == "secret" && name == "source") || ((kind == "secret" || kind == "ssh") && name == "readonly") {
			allowed = append(allowed, alias)
		}
	}
	// Stage/image sources are bound later by the planner, before execution.
	if kind == "bind" || kind == "cache" || !known {
		allowed = append(allowed, "from")
	}
	if kind == "secret" {
		allowed = append(allowed, "source")
	}
	if kind == "secret" || kind == "ssh" {
		allowed = append(allowed, "readonly")
	}
	if err := ValidateProperties(properties, allowed...); err != nil {
		return err
	}
	if known {
		for _, item := range []struct {
			name   string
			values []string
		}{
			{"sharing", []string{"shared", "private", "locked"}},
			{"relabel", []string{"private", "shared"}},
			{"bind-propagation", []string{"shared", "rshared", "private", "rprivate", "slave", "rslave"}},
		} {
			if err := validatePropertyEnum(properties, item.name, resolved, item.values...); err != nil {
				return err
			}
		}
	}
	return nil
}

func validatePropertyEnum(properties map[string]string, name string, resolved bool, allowed ...string) error {
	value, exists := properties[name]
	if !exists || value == "" || (!resolved && strings.Contains(value, "$")) {
		return nil
	}
	if !slices.Contains(allowed, value) {
		return fmt.Errorf("%s property %q is not supported; expected %s", name, value, strings.Join(allowed, ", "))
	}
	return nil
}
