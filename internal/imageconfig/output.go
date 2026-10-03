package imageconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// OutputControls are build-wide image metadata overrides. Environment values
// are applied at the beginning of every stage so they affect RUN instructions;
// the remaining values are applied only to the selected output image.
type OutputControls struct {
	Env                      []string `json:"env,omitempty"`
	Labels                   []string `json:"labels,omitempty"`
	UnsetEnv                 []string `json:"unset_env,omitempty"`
	UnsetLabels              []string `json:"unset_labels,omitempty"`
	Annotations              []string `json:"annotations,omitempty"`
	UnsetAnnotations         []string `json:"unset_annotations,omitempty"`
	DropInheritedLabels      bool     `json:"drop_inherited_labels,omitempty"`
	DropInheritedAnnotations bool     `json:"drop_inherited_annotations,omitempty"`
	OmitHistory              bool     `json:"omit_history,omitempty"`
	IdentityLabel            *bool    `json:"identity_label,omitempty"`
	LayerLabels              []string `json:"layer_labels,omitempty"`
	CreatedAnnotation        *bool    `json:"created_annotation,omitempty"`
	OSFeatures               []string `json:"os_features,omitempty"`
	OSVersion                string   `json:"os_version,omitempty"`
}

func (controls OutputControls) HasChanges() bool {
	return len(controls.Env) != 0 || len(controls.Labels) != 0 || len(controls.UnsetEnv) != 0 || len(controls.UnsetLabels) != 0 ||
		len(controls.Annotations) != 0 || len(controls.UnsetAnnotations) != 0 || controls.DropInheritedLabels ||
		controls.DropInheritedAnnotations || controls.OmitHistory || controls.IdentityLabel != nil ||
		len(controls.LayerLabels) != 0 || controls.CreatedAnnotation != nil || len(controls.OSFeatures) != 0 || controls.OSVersion != ""
}

// Validate checks the normalized KEY=VALUE and key-only lists accepted by the
// executor. CLI environment inheritance is resolved before this boundary.
func (controls OutputControls) Validate() error {
	for name, values := range map[string][]string{
		"environment": controls.Env,
		"label":       controls.Labels,
		"annotation":  controls.Annotations,
	} {
		for _, value := range values {
			key, _, found := strings.Cut(value, "=")
			if !found || invalidMetadataKey(key) {
				return fmt.Errorf("invalid %s %q: expected KEY=VALUE", name, value)
			}
		}
	}
	for name, values := range map[string][]string{
		"environment": controls.UnsetEnv,
		"label":       controls.UnsetLabels,
		"annotation":  controls.UnsetAnnotations,
	} {
		for _, key := range values {
			if invalidMetadataKey(key) {
				return fmt.Errorf("invalid unset %s key %q", name, key)
			}
		}
	}
	for _, value := range controls.LayerLabels {
		key, _, found := strings.Cut(value, "=")
		if !found || invalidMetadataKey(key) {
			return fmt.Errorf("invalid layer label %q: expected KEY=VALUE", value)
		}
	}
	for _, feature := range controls.OSFeatures {
		key := strings.TrimSuffix(feature, "-")
		if key == "" || strings.ContainsAny(key, "\x00\r\n") {
			return fmt.Errorf("invalid OS feature %q", feature)
		}
	}
	if strings.ContainsAny(controls.OSVersion, "\x00\r\n") {
		return errors.New("invalid OS version")
	}
	return nil
}

func invalidMetadataKey(key string) bool {
	return key == "" || strings.ContainsAny(key, "=\x00\r\n")
}

// PrepareStage applies controls whose Podman/Buildah semantics take effect at
// stage start. ENV instructions in the definition can override these values.
func (c *Config) PrepareStage(controls OutputControls) error {
	if c == nil {
		return errors.New("nil image configuration")
	}
	if err := controls.Validate(); err != nil {
		return err
	}
	nested, err := c.nested()
	if err != nil {
		return err
	}
	if controls.DropInheritedLabels {
		delete(nested, "Labels")
	}
	if len(controls.Env) != 0 {
		var current []string
		if err := decode(nested, "Env", &current); err != nil {
			return err
		}
		current = setEnvironment(current, controls.Env)
		if err := c.setNested(nested, "Env", current); err != nil {
			return err
		}
	} else if controls.DropInheritedLabels {
		return c.storeNested(nested)
	}
	return nil
}

// ApplyOutput applies metadata controls which only affect the selected output
// image. It intentionally runs after instruction caching.
func (c *Config) ApplyOutput(controls OutputControls) error {
	if c == nil {
		return errors.New("nil image configuration")
	}
	if err := controls.Validate(); err != nil {
		return err
	}
	if len(controls.Labels) == 0 && len(controls.UnsetEnv) == 0 && len(controls.UnsetLabels) == 0 && !controls.OmitHistory &&
		len(controls.OSFeatures) == 0 && controls.OSVersion == "" {
		return nil
	}
	if controls.OmitHistory {
		delete(c.root, "history")
	}
	nested, err := c.nested()
	if err != nil {
		return err
	}
	if len(controls.UnsetEnv) != 0 {
		var current []string
		if err := decode(nested, "Env", &current); err != nil {
			return err
		}
		for _, key := range controls.UnsetEnv {
			current = slices.DeleteFunc(current, func(value string) bool {
				name, _, _ := strings.Cut(value, "=")
				return name == key
			})
		}
		if len(current) == 0 {
			delete(nested, "Env")
		} else if err := c.setNested(nested, "Env", current); err != nil {
			return err
		}
	}
	var labels map[string]string
	if err := decode(nested, "Labels", &labels); err != nil {
		return err
	}
	if labels == nil {
		labels = make(map[string]string)
	}
	for _, item := range controls.Labels {
		key, value, _ := strings.Cut(item, "=")
		labels[key] = value
	}
	for _, key := range controls.UnsetLabels {
		delete(labels, key)
	}
	if len(labels) == 0 {
		delete(nested, "Labels")
	} else if err := c.setNested(nested, "Labels", labels); err != nil {
		return err
	}
	if err := c.storeNested(nested); err != nil {
		return err
	}
	if controls.OSVersion != "" {
		value, err := json.Marshal(controls.OSVersion)
		if err != nil {
			return err
		}
		c.root["os.version"] = value
	}
	if len(controls.OSFeatures) != 0 {
		var features []string
		if raw, ok := c.root["os.features"]; ok {
			if err := json.Unmarshal(raw, &features); err != nil {
				return fmt.Errorf("decode os.features: %w", err)
			}
		}
		for _, spec := range controls.OSFeatures {
			if strings.HasSuffix(spec, "-") {
				name := strings.TrimSuffix(spec, "-")
				features = slices.DeleteFunc(features, func(feature string) bool { return feature == name })
			} else if !slices.Contains(features, spec) {
				features = append(features, spec)
			}
		}
		if len(features) == 0 {
			delete(c.root, "os.features")
		} else {
			value, err := json.Marshal(features)
			if err != nil {
				return err
			}
			c.root["os.features"] = value
		}
	}
	return nil
}

func (c *Config) storeNested(nested map[string]json.RawMessage) error {
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

func setEnvironment(current, overrides []string) []string {
	for _, override := range overrides {
		key, value, _ := strings.Cut(override, "=")
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
	return current
}
