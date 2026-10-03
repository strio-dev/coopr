// Package imageconfig owns Coopr's executor-neutral OCI image configuration.
// It preserves safe extensions while keeping executor-owned layer provenance
// structurally valid and explicit.
package imageconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"coopr/internal/definition"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

var platformToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

var topLevelFields = canonicalFields(
	"created", "author", "architecture", "os", "os.version", "os.features", "variant", "config", "rootfs", "history",
)

var nestedConfigFields = canonicalFields(
	"User", "ExposedPorts", "Env", "Entrypoint", "Cmd", "Volumes", "WorkingDir", "Labels", "StopSignal", "ArgsEscaped", "Shell", "Healthcheck", "OnBuild",
)

var healthcheckFields = canonicalFields("Test", "Interval", "Timeout", "StartPeriod", "StartInterval", "Retries")

var rootFSFields = canonicalFields("type", "diff_ids")
var historyFields = canonicalFields("created", "created_by", "author", "comment", "empty_layer")

// Config stores the complete OCI image configuration document. Unknown fields
// are retained at the top level and inside config. rootfs and history are
// intentionally closed structures because executors rewrite them.
type Config struct {
	root   map[string]json.RawMessage
	cmdSet bool // CMD explicitly set since this stage was forked
}

// Healthcheck is Docker's runtime healthcheck image configuration. Durations
// use the Docker configuration representation: integer nanoseconds in JSON.
type Healthcheck struct {
	Test          []string      `json:",omitempty"`
	Interval      time.Duration `json:",omitempty"`
	Timeout       time.Duration `json:",omitempty"`
	StartPeriod   time.Duration `json:",omitempty"`
	StartInterval time.Duration `json:",omitempty"`
	Retries       int           `json:",omitempty"`
}

// Parse validates and preserves an OCI image configuration document. An empty
// document is accepted as scratch configuration.
func Parse(data []byte) (*Config, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return New(), nil
	}
	root, err := decodeObject(data, "image configuration", topLevelFields, false)
	if err != nil {
		return nil, fmt.Errorf("decode image configuration: %w", err)
	}
	if err := validateTopLevel(root); err != nil {
		return nil, err
	}
	return &Config{root: root}, nil
}

// New returns an empty configuration for a scratch stage.
func New() *Config { return &Config{root: make(map[string]json.RawMessage)} }

// Clone creates an independent child-stage configuration, including safe
// extensions. Stage-local CMD state is deliberately reset for the child.
func (c *Config) Clone() *Config {
	if c == nil {
		return New()
	}
	return &Config{root: cloneObject(c.root)}
}

// MarshalJSON returns the complete OCI image configuration document.
func (c *Config) MarshalJSON() ([]byte, error) {
	if c == nil || c.root == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(c.root)
}

// Apply changes image configuration as an ordered, normalized Coopr
// instruction would. Filesystem operations and build arguments are handled by
// the planner and executor.
func (c *Config) Apply(inst definition.Instruction) error {
	if c == nil {
		return errors.New("nil image configuration")
	}
	if c.root == nil {
		c.root = make(map[string]json.RawMessage)
	}
	if err := definition.Validate(&definition.Definition{Instructions: []definition.Instruction{inst}}); err != nil {
		return err
	}
	if inst.Name == "arg" {
		return nil
	}
	if len(inst.Children) != 0 {
		return fmt.Errorf("%s children are not image configuration", strings.ToUpper(inst.Name))
	}
	nested, err := c.nested()
	if err != nil {
		return err
	}
	switch inst.Name {
	case "env":
		var current []string
		if err := decode(nested, "Env", &current); err != nil {
			return err
		}
		values := properties(inst)
		for _, key := range propertyKeys(inst) {
			value := values[key]
			found := false
			updated := current[:0]
			for _, existing := range current {
				name, _, _ := strings.Cut(existing, "=")
				if name == key {
					if !found {
						updated = append(updated, key+"="+value)
						found = true
					}
					continue
				}
				updated = append(updated, existing)
			}
			current = updated
			if !found {
				current = append(current, key+"="+value)
			}
		}
		return c.setNested(nested, "Env", current)
	case "label":
		current := make(map[string]string)
		if err := decode(nested, "Labels", &current); err != nil {
			return err
		}
		if current == nil {
			current = make(map[string]string)
		}
		for key, value := range properties(inst) {
			current[key] = value
		}
		return c.setNested(nested, "Labels", current)
	case "workdir":
		if err := plain(inst, 1); err != nil {
			return err
		}
		var current string
		if err := decode(nested, "WorkingDir", &current); err != nil {
			return err
		}
		value := inst.Arguments[0]
		if !path.IsAbs(value) {
			if current == "" {
				current = "/"
			}
			value = path.Join(current, value)
		} else {
			value = path.Clean(value)
		}
		return c.setNested(nested, "WorkingDir", value)
	case "user":
		if err := plain(inst, 1); err != nil {
			return err
		}
		return c.setNested(nested, "User", inst.Arguments[0])
	case "cmd":
		if err := vector(inst, true); err != nil {
			return err
		}
		if err := c.setNested(nested, "Cmd", slices.Clone(inst.Arguments)); err != nil {
			return err
		}
		c.cmdSet = true
		return nil
	case "entrypoint":
		if err := vector(inst, true); err != nil {
			return err
		}
		if !c.cmdSet {
			delete(nested, "Cmd")
		}
		return c.setNested(nested, "Entrypoint", slices.Clone(inst.Arguments))
	case "shell":
		if err := vector(inst, false); err != nil {
			return err
		}
		return c.setNested(nested, "Shell", slices.Clone(inst.Arguments))
	case "stopsignal":
		if err := plain(inst, 1); err != nil {
			return err
		}
		return c.setNested(nested, "StopSignal", inst.Arguments[0])
	case "expose":
		if err := plain(inst, 1); err != nil {
			return err
		}
		if strings.ContainsAny(inst.Arguments[0], " \t\r\n\x00") {
			return fmt.Errorf("EXPOSE port %q must be one token", inst.Arguments[0])
		}
		current := make(map[string]struct{})
		if err := decode(nested, "ExposedPorts", &current); err != nil {
			return err
		}
		if current == nil {
			current = make(map[string]struct{})
		}
		current[inst.Arguments[0]] = struct{}{}
		return c.setNested(nested, "ExposedPorts", current)
	case "volume":
		if err := plain(inst, 1); err != nil {
			return err
		}
		volume := strings.TrimSpace(inst.Arguments[0])
		if volume == "" || strings.ContainsRune(volume, '\x00') {
			return fmt.Errorf("VOLUME path %q is invalid", inst.Arguments[0])
		}
		current := make(map[string]struct{})
		if err := decode(nested, "Volumes", &current); err != nil {
			return err
		}
		if current == nil {
			current = make(map[string]struct{})
		}
		current[volume] = struct{}{}
		return c.setNested(nested, "Volumes", current)
	case "maintainer":
		if err := plain(inst, 1); err != nil {
			return err
		}
		maintainer := inst.Arguments[0]
		if strings.TrimSpace(maintainer) == "" || strings.ContainsAny(maintainer, "\r\n\x00") {
			return errors.New("MAINTAINER requires one nonempty single-line value")
		}
		return setRaw(c.root, "author", maintainer)
	case "healthcheck":
		healthcheck, err := healthcheckFromInstruction(inst)
		if err != nil {
			return err
		}
		return c.setNested(nested, "Healthcheck", healthcheck)
	case "onbuild":
		var current []string
		if err := decode(nested, "OnBuild", &current); err != nil {
			return err
		}
		current = append(current, inst.Arguments[0])
		return c.setNested(nested, "OnBuild", current)
	default:
		return fmt.Errorf("%s is not an image configuration instruction", inst.Name)
	}
}

// Healthcheck returns an independent typed copy of the Docker healthcheck.
// A nil result means no healthcheck is configured.
func (c *Config) Healthcheck() (*Healthcheck, error) {
	if c == nil {
		return nil, nil
	}
	nested, err := c.nested()
	if err != nil {
		return nil, err
	}
	raw, exists := nested["Healthcheck"]
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var healthcheck Healthcheck
	if err := json.Unmarshal(raw, &healthcheck); err != nil {
		return nil, fmt.Errorf("decode config.Healthcheck: %w", err)
	}
	healthcheck.Test = slices.Clone(healthcheck.Test)
	return &healthcheck, nil
}

// OnBuild returns an independent copy of the inherited Docker trigger list.
func (c *Config) OnBuild() ([]string, error) {
	if c == nil {
		return nil, nil
	}
	nested, err := c.nested()
	if err != nil {
		return nil, err
	}
	var onBuild []string
	if err := decode(nested, "OnBuild", &onBuild); err != nil {
		return nil, err
	}
	return slices.Clone(onBuild), nil
}

// ClearOnBuild removes inherited triggers from this configuration. Clone the
// parent configuration first when consuming triggers for a child stage.
func (c *Config) ClearOnBuild() error {
	if c == nil {
		return errors.New("nil image configuration")
	}
	nested, err := c.nested()
	if err != nil {
		return err
	}
	delete(nested, "OnBuild")
	raw, err := json.Marshal(nested)
	if err != nil {
		return err
	}
	if c.root == nil {
		c.root = make(map[string]json.RawMessage)
	}
	c.root["config"] = raw
	return nil
}

// Shell returns the configured shell or a copy of fallback when the image does
// not define one. The returned slice never aliases Config or fallback.
func (c *Config) Shell(fallback []string) ([]string, error) {
	if c == nil {
		return slices.Clone(fallback), nil
	}
	nested, err := c.nested()
	if err != nil {
		return nil, err
	}
	if _, exists := nested["Shell"]; !exists {
		return slices.Clone(fallback), nil
	}
	if bytes.Equal(bytes.TrimSpace(nested["Shell"]), []byte("null")) {
		return slices.Clone(fallback), nil
	}
	var shell []string
	if err := decode(nested, "Shell", &shell); err != nil {
		return nil, err
	}
	if len(shell) == 0 {
		return nil, errors.New("config.Shell must contain at least one argument")
	}
	return slices.Clone(shell), nil
}

// AdoptExecutorProvenance takes the emitted config's executor-owned fields and
// merges them into the normalized raw config. Safe extension fields and
// normalized config values stay authoritative, and stage-local CMD state is
// retained because the receiver itself is not replaced.
func (c *Config) AdoptExecutorProvenance(emitted []byte) error {
	if c == nil {
		return errors.New("nil image configuration")
	}
	executor, err := Parse(emitted)
	if err != nil {
		return fmt.Errorf("executor image configuration: %w", err)
	}
	if _, exists := executor.root["rootfs"]; !exists {
		return errors.New("executor image configuration has no rootfs")
	}
	want, wantSet, err := platform(c.root)
	if err != nil {
		return err
	}
	got, gotSet, err := platform(executor.root)
	if err != nil {
		return err
	}
	if !gotSet {
		return errors.New("executor image configuration has no platform")
	}
	if wantSet && !samePlatform(want, got) {
		return fmt.Errorf("executor platform %s/%s does not match image platform %s/%s", got.OS, got.Architecture, want.OS, want.Architecture)
	}

	next := cloneObject(c.root)
	for _, key := range []string{"created", "architecture", "os", "os.version", "os.features", "variant", "rootfs", "history"} {
		if value, exists := executor.root[key]; exists {
			next[key] = bytes.Clone(value)
		} else {
			delete(next, key)
		}
	}
	c.root = next
	return nil
}

// FlattenPackage rewrites filesystem provenance for a package snapshot that
// contains exactly one flattened layer. Existing history remains as metadata
// and is marked empty because it no longer corresponds to distinct layers.
func (c *Config) FlattenPackage(layer digest.Digest, created *time.Time) error {
	if c == nil {
		return errors.New("nil image configuration")
	}
	if err := layer.Validate(); err != nil {
		return fmt.Errorf("package layer digest: %w", err)
	}
	var history []v1.History
	if raw, exists := c.root["history"]; exists {
		if err := json.Unmarshal(raw, &history); err != nil {
			return fmt.Errorf("decode history: %w", err)
		}
	}
	for index := range history {
		history[index].EmptyLayer = true
	}
	history = append(history, v1.History{Created: created, CreatedBy: "coopr package snapshot"})
	rootFS := v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{layer}}
	rootRaw, err := json.Marshal(rootFS)
	if err != nil {
		return err
	}
	historyRaw, err := json.Marshal(history)
	if err != nil {
		return err
	}
	c.root["rootfs"] = rootRaw
	c.root["history"] = historyRaw
	return nil
}

func validateTopLevel(root map[string]json.RawMessage) error {
	if raw, exists := root["config"]; exists {
		nested, err := decodeObject(raw, "config", nestedConfigFields, false)
		if err != nil {
			return fmt.Errorf("image configuration config: %w", err)
		}
		if err := validateNested(nested); err != nil {
			return err
		}
	}
	if raw, exists := root["rootfs"]; exists {
		if err := validateRootFS(raw); err != nil {
			return err
		}
	}
	if raw, exists := root["history"]; exists {
		if err := validateHistory(raw); err != nil {
			return err
		}
	}
	if err := validateOptionalTime(root, "created", "image configuration created"); err != nil {
		return err
	}
	for _, key := range []string{"author", "architecture", "os", "os.version", "variant"} {
		if err := validateOptional(root, key, new(string)); err != nil {
			return err
		}
	}
	if err := validateOptional(root, "os.features", new([]string)); err != nil {
		return err
	}
	_, _, err := platform(root)
	return err
}

func validateNested(nested map[string]json.RawMessage) error {
	checks := []struct {
		key string
		dst any
	}{
		{"User", new(string)}, {"ExposedPorts", new(map[string]struct{})}, {"Env", new([]string)},
		{"Entrypoint", new([]string)}, {"Cmd", new([]string)}, {"Volumes", new(map[string]struct{})},
		{"WorkingDir", new(string)}, {"Labels", new(map[string]string)}, {"StopSignal", new(string)},
		{"ArgsEscaped", new(bool)}, {"Shell", new([]string)},
		{"OnBuild", new([]string)},
	}
	if raw, exists := nested["Healthcheck"]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if _, err := decodeObject(raw, "config.Healthcheck", healthcheckFields, false); err != nil {
			return fmt.Errorf("image configuration config.Healthcheck: %w", err)
		}
		var healthcheck Healthcheck
		if err := json.Unmarshal(raw, &healthcheck); err != nil {
			return fmt.Errorf("image configuration config.Healthcheck: %w", err)
		}
		if err := validateHealthcheck(healthcheck); err != nil {
			return fmt.Errorf("image configuration config.Healthcheck: %w", err)
		}
	}
	if raw, exists := nested["OnBuild"]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var onBuild []string
		if err := json.Unmarshal(raw, &onBuild); err != nil {
			return fmt.Errorf("image configuration config.OnBuild: %w", err)
		}
		for index, trigger := range onBuild {
			if strings.TrimSpace(trigger) == "" || strings.ContainsRune(trigger, '\x00') {
				return fmt.Errorf("image configuration config.OnBuild[%d] must be a nonempty Dockerfile instruction without NUL", index)
			}
		}
	}
	for _, check := range checks {
		if err := validateOptionalNullable(nested, check.key, check.dst); err != nil {
			return fmt.Errorf("image configuration config.%s: %w", check.key, err)
		}
	}
	if raw, exists := nested["Shell"]; exists {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil
		}
		var shell []string
		if err := json.Unmarshal(raw, &shell); err != nil || len(shell) == 0 {
			return errors.New("image configuration config.Shell must contain at least one argument")
		}
	}
	return nil
}

func validateHealthcheck(healthcheck Healthcheck) error {
	if len(healthcheck.Test) == 0 {
		return errors.New("test must contain CMD, CMD-SHELL, or NONE")
	}
	for _, argument := range healthcheck.Test {
		if strings.ContainsRune(argument, '\x00') {
			return errors.New("test cannot contain NUL")
		}
	}
	switch healthcheck.Test[0] {
	case "NONE":
		if len(healthcheck.Test) != 1 || healthcheck.Interval != 0 || healthcheck.Timeout != 0 || healthcheck.StartPeriod != 0 || healthcheck.StartInterval != 0 || healthcheck.Retries != 0 {
			return errors.New("NONE does not accept command arguments or timing fields")
		}
	case "CMD":
		if len(healthcheck.Test) < 2 {
			return errors.New("CMD requires at least one command argument")
		}
	case "CMD-SHELL":
		if len(healthcheck.Test) != 2 {
			return errors.New("CMD-SHELL requires exactly one command")
		}
	default:
		return errors.New("test must begin with CMD, CMD-SHELL, or NONE")
	}
	for name, duration := range map[string]time.Duration{
		"Interval": healthcheck.Interval, "Timeout": healthcheck.Timeout,
		"StartPeriod": healthcheck.StartPeriod, "StartInterval": healthcheck.StartInterval,
	} {
		if duration != 0 && duration < time.Millisecond {
			return fmt.Errorf("%s cannot be less than 1ms", name)
		}
	}
	if healthcheck.Retries < 0 {
		return errors.New("retries must be nonnegative")
	}
	return nil
}

func healthcheckFromInstruction(inst definition.Instruction) (*Healthcheck, error) {
	if len(inst.Arguments) == 1 && inst.Arguments[0] == "NONE" {
		return &Healthcheck{Test: []string{"NONE"}}, nil
	}
	healthcheck := &Healthcheck{Test: slices.Clone(inst.Arguments)}
	for name, target := range map[string]*time.Duration{
		"interval": &healthcheck.Interval, "timeout": &healthcheck.Timeout,
		"start-period": &healthcheck.StartPeriod, "start-interval": &healthcheck.StartInterval,
	} {
		value, exists := inst.Properties[name]
		if !exists {
			continue
		}
		duration, err := time.ParseDuration(value)
		if err != nil {
			return nil, fmt.Errorf("HEALTHCHECK %s: %w", name, err)
		}
		*target = duration
	}
	if value, exists := inst.Properties["retries"]; exists {
		retries, err := strconv.Atoi(value)
		if err != nil {
			return nil, fmt.Errorf("HEALTHCHECK retries: %w", err)
		}
		healthcheck.Retries = retries
	}
	return healthcheck, nil
}

func validateRootFS(raw json.RawMessage) error {
	object, err := decodeObject(raw, "rootfs", rootFSFields, true)
	if err != nil {
		return fmt.Errorf("image configuration rootfs: %w", err)
	}
	if _, exists := object["type"]; !exists {
		return errors.New("image configuration rootfs is missing type")
	}
	if _, exists := object["diff_ids"]; !exists {
		return errors.New("image configuration rootfs is missing diff_ids")
	}
	var rootFS v1.RootFS
	if err := json.Unmarshal(raw, &rootFS); err != nil {
		return fmt.Errorf("image configuration rootfs: %w", err)
	}
	if rootFS.Type != "layers" {
		return fmt.Errorf("image configuration rootfs type %q is unsupported", rootFS.Type)
	}
	for index, id := range rootFS.DiffIDs {
		if err := id.Validate(); err != nil {
			return fmt.Errorf("image configuration rootfs diff_ids[%d]: %w", index, err)
		}
	}
	return nil
}

func validateHistory(raw json.RawMessage) error {
	var entries []json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '[' || json.Unmarshal(raw, &entries) != nil || entries == nil {
		return errors.New("image configuration history must be an array")
	}
	for index, entry := range entries {
		object, err := decodeObject(entry, fmt.Sprintf("history[%d]", index), historyFields, true)
		if err != nil {
			return fmt.Errorf("image configuration history[%d]: %w", index, err)
		}
		if err := validateOptionalTime(object, "created", fmt.Sprintf("image configuration history[%d].created", index)); err != nil {
			return err
		}
		for _, key := range []string{"created_by", "author", "comment"} {
			if err := validateOptional(object, key, new(string)); err != nil {
				return fmt.Errorf("image configuration history[%d].%s: %w", index, key, err)
			}
		}
		if err := validateOptional(object, "empty_layer", new(bool)); err != nil {
			return fmt.Errorf("image configuration history[%d].empty_layer: %w", index, err)
		}
	}
	return nil
}

func validateOptionalNullable(object map[string]json.RawMessage, key string, dst any) error {
	raw, exists := object[key]
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

func validateOptional(object map[string]json.RawMessage, key string, dst any) error {
	raw, exists := object[key]
	if !exists {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if err == nil {
			err = errors.New("must not be null")
		}
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

func validateOptionalTime(object map[string]json.RawMessage, key, label string) error {
	if _, exists := object[key]; !exists {
		return nil
	}
	var value string
	if err := validateOptional(object, key, &value); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

func platform(root map[string]json.RawMessage) (v1.Platform, bool, error) {
	var result v1.Platform
	for key, target := range map[string]*string{
		"architecture": &result.Architecture, "os": &result.OS, "os.version": &result.OSVersion, "variant": &result.Variant,
	} {
		if raw, exists := root[key]; exists {
			if err := json.Unmarshal(raw, target); err != nil {
				return v1.Platform{}, false, fmt.Errorf("image configuration %s: %w", key, err)
			}
		}
	}
	if raw, exists := root["os.features"]; exists {
		if err := json.Unmarshal(raw, &result.OSFeatures); err != nil {
			return v1.Platform{}, false, fmt.Errorf("image configuration os.features: %w", err)
		}
	}
	set := result.Architecture != "" || result.OS != "" || result.OSVersion != "" || result.Variant != "" || len(result.OSFeatures) != 0
	if !set {
		return v1.Platform{}, false, nil
	}
	if result.Architecture == "" || result.OS == "" {
		return v1.Platform{}, false, errors.New("image configuration architecture and os must either both be set or both be omitted")
	}
	if !platformToken.MatchString(result.Architecture) {
		return v1.Platform{}, false, fmt.Errorf("image configuration has invalid architecture %q", result.Architecture)
	}
	if !platformToken.MatchString(result.OS) {
		return v1.Platform{}, false, fmt.Errorf("image configuration has invalid os %q", result.OS)
	}
	if result.Variant != "" && !platformToken.MatchString(result.Variant) {
		return v1.Platform{}, false, fmt.Errorf("image configuration has invalid variant %q", result.Variant)
	}
	return result, true, nil
}

func samePlatform(left, right v1.Platform) bool {
	normalizeVariant := func(value v1.Platform) string {
		if value.Architecture == "arm64" && (value.Variant == "" || value.Variant == "v8") {
			return ""
		}
		return value.Variant
	}
	if left.OS != right.OS || left.Architecture != right.Architecture || left.OSVersion != right.OSVersion || normalizeVariant(left) != normalizeVariant(right) {
		return false
	}
	leftFeatures := slices.Clone(left.OSFeatures)
	rightFeatures := slices.Clone(right.OSFeatures)
	slices.Sort(leftFeatures)
	slices.Sort(rightFeatures)
	return slices.Equal(leftFeatures, rightFeatures)
}

func (c *Config) nested() (map[string]json.RawMessage, error) {
	if c == nil || c.root == nil {
		return make(map[string]json.RawMessage), nil
	}
	if raw, exists := c.root["config"]; exists {
		return decodeObject(raw, "config", nestedConfigFields, false)
	}
	return make(map[string]json.RawMessage), nil
}

func decode(object map[string]json.RawMessage, key string, result any) error {
	if raw, exists := object[key]; exists {
		if err := json.Unmarshal(raw, result); err != nil {
			return fmt.Errorf("decode config.%s: %w", key, err)
		}
	}
	return nil
}

func (c *Config) setNested(nested map[string]json.RawMessage, key string, value any) error {
	if err := setRaw(nested, key, value); err != nil {
		return err
	}
	raw, err := json.Marshal(nested)
	if err != nil {
		return err
	}
	c.root["config"] = raw
	return nil
}

func setRaw(object map[string]json.RawMessage, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	object[key] = raw
	return nil
}

func plain(inst definition.Instruction, arguments int) error {
	if len(inst.Arguments) != arguments || len(inst.Properties) != 0 || len(inst.Children) != 0 || inst.Form != "" {
		return fmt.Errorf("%s requires %d plain argument(s)", strings.ToUpper(inst.Name), arguments)
	}
	return nil
}

func vector(inst definition.Instruction, allowEmpty bool) error {
	if len(inst.Properties) != 0 || len(inst.Children) != 0 || inst.Form != "" && inst.Form != "exec" {
		return fmt.Errorf("%s requires normalized exec arguments", strings.ToUpper(inst.Name))
	}
	if !allowEmpty && len(inst.Arguments) == 0 {
		return fmt.Errorf("%s requires at least one argument", strings.ToUpper(inst.Name))
	}
	return nil
}

func properties(inst definition.Instruction) map[string]string {
	if len(inst.Properties) != 0 {
		return inst.Properties
	}
	return map[string]string{inst.Arguments[0]: inst.Arguments[1]}
}

func propertyKeys(inst definition.Instruction) []string {
	props := properties(inst)
	keys := make([]string, 0, len(props))
	for key := range props {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func canonicalFields(names ...string) map[string]string {
	result := make(map[string]string, len(names))
	for _, name := range names {
		result[strings.ToLower(name)] = name
	}
	return result
}

func decodeObject(raw []byte, label string, fields map[string]string, rejectUnknown bool) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, fmt.Errorf("%s must be an object", label)
	}
	result := make(map[string]json.RawMessage)
	seenCanonical := make(map[string]string)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("%s has a non-string key", label)
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("%s has duplicate key %q", label, key)
		}
		canonical, known := fields[strings.ToLower(key)]
		if known {
			if key != canonical {
				return nil, fmt.Errorf("%s field %q must be spelled %q", label, key, canonical)
			}
			if previous, exists := seenCanonical[canonical]; exists {
				return nil, fmt.Errorf("%s fields %q and %q collide", label, previous, key)
			}
			seenCanonical[canonical] = key
		} else if rejectUnknown {
			return nil, fmt.Errorf("%s has unsupported field %q", label, key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("%s field %q: %w", label, key, err)
		}
		result[key] = bytes.Clone(value)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if err := ensureEOF(decoder); err != nil {
		return nil, err
	}
	return result, nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON")
		}
		return err
	}
	return nil
}

func cloneObject(source map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(source))
	for key, raw := range source {
		result[key] = bytes.Clone(raw)
	}
	return result
}
