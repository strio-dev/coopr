package buildah

import (
	"encoding/hex"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"coopr/internal/planner"
)

// RunMount is one transient mount for a RUN. Properties retain the authored
// mount vocabulary until apply time, where it is validated and serialized for
// Buildah's Builder.Run API.
type RunMount struct {
	Type       string
	Properties map[string]string
	// BoundFrom marks a graph-resolved, immutable storage image ID. Authored
	// from= values never reach Buildah without this executor-owned binding.
	BoundFrom bool
	// PrivateRelabel is executor-owned. Filtered context snapshots are private
	// to one RUN and require an SELinux label usable by that container.
	PrivateRelabel bool
}

func lowerRunMounts(operation planner.Operation, resolve CacheMountIDResolver, boundFrom map[int]string) ([]RunMount, error) {
	mounts := make([]RunMount, 0, len(operation.Children))
	for index, child := range operation.Children {
		if child.Name == "device" {
			continue
		}
		if child.Name != "mount" || len(child.Arguments) != 1 || child.Form != "" || len(child.Children) != 0 {
			return nil, fmt.Errorf("RUN child %d is not a supported mount", index+1)
		}
		mountType := child.Arguments[0]
		properties, err := normalizeRunMountProperties(mountType, child.Properties)
		if err != nil {
			return nil, fmt.Errorf("RUN mount %d: %w", index+1, err)
		}
		imageID := boundFrom[index]
		if properties["from"] != "" {
			if imageID == "" {
				return nil, fmt.Errorf("RUN mount %d: %s mounts from stages or images are not supported until their sources are bound", index+1, mountType)
			}
			properties["from"] = imageID
		} else if imageID != "" {
			return nil, fmt.Errorf("RUN mount %d: unexpected bound source", index+1)
		}
		switch mountType {
		case "bind":
			if imageID == "" && operation.MountContexts[index] == "" {
				return nil, fmt.Errorf("RUN mount %d: bind mount has no validated context", index+1)
			}
		case "cache":
			if properties["id"] == "" {
				if resolve == nil {
					return nil, fmt.Errorf("RUN mount %d: cache mount without id requires executor scope", index+1)
				}
				id, err := resolve(operation, index)
				if err != nil {
					return nil, fmt.Errorf("RUN mount %d: resolve cache mount id: %w", index+1, err)
				}
				if id == "" {
					return nil, fmt.Errorf("RUN mount %d: cache mount ID resolver returned an empty id", index+1)
				}
				properties["id"] = id
			}
		case "tmpfs":
		case "secret", "ssh":
		default:
			return nil, fmt.Errorf("RUN mount %d: mount type %q is not supported", index+1, mountType)
		}
		mounts = append(mounts, RunMount{Type: mountType, Properties: properties, BoundFrom: imageID != ""})
	}
	return mounts, nil
}

func serializeRunMounts(mounts []RunMount, contextDir string) ([]string, error) {
	result := make([]string, 0, len(mounts))
	for index, mount := range mounts {
		serialized, err := serializeRunMount(mount, contextDir)
		if err != nil {
			return nil, fmt.Errorf("RUN mount %d: %w", index+1, err)
		}
		result = append(result, serialized)
	}
	return result, nil
}

func serializeRunMount(mount RunMount, contextDir string) (string, error) {
	properties, err := normalizeRunMountProperties(mount.Type, mount.Properties)
	if err != nil {
		return "", err
	}
	mount.Properties = properties
	var allowed map[string]bool
	switch mount.Type {
	case "bind":
		allowed = propertySet("source", "target", "readonly")
	case "cache":
		allowed = propertySet("id", "source", "target", "readonly", "sharing", "mode", "uid", "gid")
	case "tmpfs":
		allowed = propertySet("target", "readonly", "size", "mode")
	case "secret":
		allowed = propertySet("id", "target", "required", "mode", "uid", "gid", "env")
	case "ssh":
		allowed = propertySet("id", "target", "required", "mode", "uid", "gid")
	default:
		return "", fmt.Errorf("mount type %q is not supported", mount.Type)
	}
	if mount.BoundFrom {
		imageID := mount.Properties["from"]
		if len(imageID) != 64 {
			return "", fmt.Errorf("bound %s mount requires an immutable storage image ID", mount.Type)
		}
		decoded := make([]byte, 32)
		if _, err := hex.Decode(decoded, []byte(imageID)); err != nil || hex.EncodeToString(decoded) != imageID {
			return "", fmt.Errorf("bound %s mount requires an immutable storage image ID", mount.Type)
		}
		allowed["from"] = true
	}
	for name, value := range mount.Properties {
		if !allowed[name] {
			if name == "from" {
				return "", fmt.Errorf("%s mounts from stages or images are not supported until their sources are bound", mount.Type)
			}
			return "", fmt.Errorf("%s mount property %q is not supported", mount.Type, name)
		}
		if value == "" || strings.ContainsAny(value, ",\x00\r\n") {
			return "", fmt.Errorf("%s mount property %q has an unsafe or ambiguous value", mount.Type, name)
		}
	}
	target := mount.Properties["target"]
	if target == "" && mount.Type != "secret" && mount.Type != "ssh" {
		return "", fmt.Errorf("%s mount requires target", mount.Type)
	}
	if (mount.Type == "bind" || mount.Type == "cache" || mount.Type == "tmpfs") && path.Clean(target) == "/" {
		return "", fmt.Errorf("%s mount target must not be /", mount.Type)
	}
	if mount.Type == "secret" && mount.Properties["id"] == "" && target == "" {
		return "", fmt.Errorf("secret mount requires id or target")
	}
	if mount.Type == "secret" || mount.Type == "ssh" {
		if required := mount.Properties["required"]; required != "" {
			if _, err := strconv.ParseBool(required); err != nil {
				return "", fmt.Errorf("%s mount required must be true or false", mount.Type)
			}
		}
		if err := validateUnsignedMountProperty(mount.Properties, "uid", 10); err != nil {
			return "", err
		}
		if err := validateUnsignedMountProperty(mount.Properties, "gid", 10); err != nil {
			return "", err
		}
	}
	if mount.Type == "bind" {
		if !mount.BoundFrom && contextDir == "" {
			return "", fmt.Errorf("bind mount requires a build context")
		}
		if !mount.BoundFrom {
			source, err := normalizeContextMountSource(mount.Properties["source"])
			if err != nil {
				return "", err
			}
			if source != "" {
				mount.Properties["source"] = source
			}
		} else if err := validateStageMountSource(mount.Properties["source"]); err != nil {
			return "", err
		}
	}
	if mount.Type == "cache" {
		if mount.Properties["id"] == "" {
			return "", fmt.Errorf("cache mount requires a resolved id")
		}
		if sharing := mount.Properties["sharing"]; sharing != "" && sharing != "shared" && sharing != "private" && sharing != "locked" {
			return "", fmt.Errorf("cache mount sharing %q is not supported", sharing)
		}
		if err := validateUnsignedMountProperty(mount.Properties, "uid", 10); err != nil {
			return "", err
		}
		if err := validateUnsignedMountProperty(mount.Properties, "gid", 10); err != nil {
			return "", err
		}
	}
	if err := validateUnsignedMountProperty(mount.Properties, "mode", 8); err != nil {
		return "", err
	}

	fields := []string{"type=" + mount.Type}
	if mount.PrivateRelabel {
		if mount.Type != "bind" || mount.BoundFrom {
			return "", fmt.Errorf("private relabel requires a context bind mount")
		}
		fields = append(fields, "relabel=private")
	}
	keys := make([]string, 0, len(mount.Properties))
	for name := range mount.Properties {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		value := mount.Properties[name]
		switch name {
		case "readonly":
			enabled, err := strconv.ParseBool(value)
			if err != nil {
				return "", fmt.Errorf("%s mount readonly must be true or false", mount.Type)
			}
			if enabled {
				fields = append(fields, "readonly")
			} else if mount.Type == "bind" {
				fields = append(fields, "readwrite")
			}
		case "size":
			fields = append(fields, "tmpfs-size="+value)
		case "mode":
			if mount.Type == "tmpfs" {
				fields = append(fields, "tmpfs-mode="+value)
			} else {
				fields = append(fields, name+"="+value)
			}
		default:
			fields = append(fields, name+"="+value)
		}
	}
	return strings.Join(fields, ","), nil
}

func normalizeRunMountProperties(mountType string, authored map[string]string) (map[string]string, error) {
	properties := make(map[string]string, len(authored))
	aliases := map[string]string{
		"src": "source", "dst": "target", "destination": "target", "ro": "readonly",
	}
	for name, value := range authored {
		canonical := name
		if alias, ok := aliases[name]; ok {
			canonical = alias
		}
		if name == "rw" || name == "readwrite" {
			enabled, err := strconv.ParseBool(value)
			if err != nil {
				return nil, fmt.Errorf("%s mount %s must be true or false", mountType, name)
			}
			canonical = "readonly"
			value = strconv.FormatBool(!enabled)
		}
		if _, duplicate := properties[canonical]; duplicate {
			return nil, fmt.Errorf("%s mount properties contain duplicate %q aliases", mountType, canonical)
		}
		properties[canonical] = value
	}
	if mountType == "secret" {
		if source := properties["source"]; source != "" {
			if properties["id"] != "" {
				return nil, fmt.Errorf("secret mount cannot specify both source and id")
			}
			properties["id"] = source
			delete(properties, "source")
		}
	}
	if mountType == "secret" || mountType == "ssh" {
		if readonly, ok := properties["readonly"]; ok {
			if _, err := strconv.ParseBool(readonly); err != nil {
				return nil, fmt.Errorf("%s mount readonly must be true or false", mountType)
			}
			// BuildKit ignores the flag for these inherently read-only mounts;
			// Buildah's secret/SSH parsers do not accept it.
			delete(properties, "readonly")
		}
	}
	return properties, nil
}

func propertySet(names ...string) map[string]bool {
	result := make(map[string]bool, len(names))
	for _, name := range names {
		result[name] = true
	}
	return result
}

func normalizeContextMountSource(source string) (string, error) {
	if source == "" {
		return "", nil
	}
	if filepath.IsAbs(source) {
		// BuildKit interprets an absolute bind source inside the selected build
		// context.  Strip the context-root slash only after cleaning so paths such
		// as /../../host stay confined to the context instead of becoming host
		// paths when handed to Buildah.
		cleaned := strings.TrimPrefix(filepath.Clean(source), string(filepath.Separator))
		if cleaned == "" {
			return ".", nil
		}
		return cleaned, nil
	}
	cleaned := filepath.Clean(source)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("bind mount source %q escapes the build context", source)
	}
	return cleaned, nil
}

func validateStageMountSource(source string) error {
	cleaned := filepath.Clean(strings.TrimPrefix(source, string(filepath.Separator)))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("bind mount source %q escapes the source stage", source)
	}
	return nil
}

func validateUnsignedMountProperty(properties map[string]string, name string, base int) error {
	value := properties[name]
	if value == "" {
		return nil
	}
	if _, err := strconv.ParseUint(value, base, 32); err != nil {
		return fmt.Errorf("mount property %q has invalid value %q", name, value)
	}
	return nil
}
