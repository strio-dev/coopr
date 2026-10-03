// Package cache stores portable logical transformation results as OCI artifacts.
package cache

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"coopr/internal/planner"
	"coopr/internal/stateidentity"
	"github.com/containerd/platforms"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const KeyVersion = "coopr.strio.dev/cache-key/v1"

// Key names only measured, immutable inputs. Callers must refuse cache reuse
// for undeclared network, secrets, SSH, or mutable mount dependent outputs.
type Key struct {
	Input       stateidentity.Identity   `json:"input"`
	Component   digest.Digest            `json:"component,omitempty"`
	Instruction string                   `json:"instruction,omitempty"`
	Parameters  map[string]string        `json:"parameters,omitempty"`
	Packages    map[string]v1.Descriptor `json:"packages,omitempty"`
	Inputs      map[string]digest.Digest `json:"inputs,omitempty"`
	Platform    v1.Platform              `json:"platform"`
	Executor    string                   `json:"executor"`
	Frontend    string                   `json:"frontend"`
	Lowering    string                   `json:"lowering"`
}

func (k Key) Validate() error {
	for name, value := range map[string]digest.Digest{"input filesystem": k.Input.Filesystem, "input configuration": k.Input.Configuration, "input state": k.Input.State} {
		if err := validSHA(value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if (k.Component == "") == (k.Instruction == "") {
		return errors.New("exactly one component digest or instruction is required")
	}
	if k.Component != "" {
		if err := validSHA(k.Component); err != nil {
			return fmt.Errorf("component: %w", err)
		}
	}
	if !utf8.ValidString(k.Instruction) || strings.ContainsRune(k.Instruction, 0) {
		return errors.New("invalid instruction")
	}
	if _, err := planner.NormalizeParameters(k.Parameters); err != nil {
		return err
	}
	for name, desc := range k.Packages {
		if err := validName(name); err != nil {
			return err
		}
		if err := validDescriptor(desc); err != nil {
			return fmt.Errorf("package %q: %w", name, err)
		}
	}
	for name, value := range k.Inputs {
		if err := validName(name); err != nil {
			return err
		}
		if err := validSHA(value); err != nil {
			return fmt.Errorf("input %q: %w", name, err)
		}
	}
	if err := validPlatform(k.Platform); err != nil {
		return err
	}
	for _, value := range []string{k.Executor, k.Frontend, k.Lowering} {
		if err := validName(value); err != nil {
			return fmt.Errorf("invalid semantic version: %w", err)
		}
	}
	return nil
}

func validName(s string) error {
	if s == "" || !utf8.ValidString(s) || strings.ContainsRune(s, 0) || strings.TrimSpace(s) != s {
		return fmt.Errorf("invalid name %q", s)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("invalid name %q", s)
		}
	}
	return nil
}
func validSHA(d digest.Digest) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if d.Algorithm() != digest.SHA256 {
		return fmt.Errorf("expected SHA-256 digest %s", d)
	}
	return nil
}
func validDescriptor(d v1.Descriptor) error {
	if err := validSHA(d.Digest); err != nil {
		return err
	}
	if d.MediaType == "" || d.Size < 0 || len(d.URLs) != 0 || len(d.Annotations) != 0 || len(d.Data) != 0 || d.Platform != nil || d.ArtifactType != "" {
		return errors.New("descriptor must contain only media type, digest and nonnegative size")
	}
	return nil
}
func validPlatform(p v1.Platform) error {
	if p.OS != "linux" || p.Architecture == "" {
		return fmt.Errorf("invalid platform %s/%s", p.OS, p.Architecture)
	}
	for _, s := range append([]string{p.Architecture, p.Variant, p.OSVersion}, p.OSFeatures...) {
		if s != "" {
			if err := validName(s); err != nil {
				return err
			}
		}
	}
	seen := map[string]bool{}
	for _, s := range p.OSFeatures {
		if s == "" || seen[s] {
			return errors.New("invalid or duplicate platform feature")
		}
		seen[s] = true
	}
	return nil
}

func normalizePlatform(p v1.Platform) v1.Platform {
	p = platforms.Normalize(p)
	p.OSFeatures = slices.Clone(p.OSFeatures)
	slices.Sort(p.OSFeatures)
	return p
}

func samePlatform(a, b v1.Platform) bool {
	a = normalizePlatform(a)
	b = normalizePlatform(b)
	return a.OS == b.OS && a.Architecture == b.Architecture && a.Variant == b.Variant && a.OSVersion == b.OSVersion && slices.Equal(a.OSFeatures, b.OSFeatures)
}

// Digest is stable across map order, OS feature order, and equivalent arm64/v8.
func (k Key) Digest() (digest.Digest, error) {
	if err := k.Validate(); err != nil {
		return "", err
	}
	k.Platform = normalizePlatform(k.Platform)
	params, err := planner.NormalizeParameters(k.Parameters)
	if err != nil {
		return "", err
	}
	k.Parameters = map[string]string{}
	var normalized struct {
		Version    string          `json:"version"`
		Key        Key             `json:"key"`
		Parameters json.RawMessage `json:"parameters"`
	}
	normalized.Version = KeyVersion
	normalized.Key = k
	normalized.Parameters = params
	data, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return digest.Digest(fmt.Sprintf("sha256:%x", sum)), nil
}
