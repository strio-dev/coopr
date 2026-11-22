package buildah

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	"github.com/strio-dev/coopr/internal/planner"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/copier"
)

// capturedCopyInput selects bytes already hashed during an owned capture. The
// caller keys the complete operation and parent dependency separately. Its
// capture must remain private and unmodified through execution; ContextPrepared
// alone is insufficient, and writable context mounts require a new capture.
func capturedCopyInput(ctx context.Context, capture capturedContext, policy contextPolicy, operation planner.Operation) (digest.Digest, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if operation.Name != "copy" && operation.Name != "add" || operation.Properties["from"] != "" {
		return "", errors.New("captured COPY/ADD input requires a context source")
	}
	lowered, _, err := lowerCopyOrAdd(operation.Instruction, "build")
	if err != nil {
		return "", err
	}
	var sources []string
	var options upstream.AddAndCopyOptions
	switch copied := lowered[0].(type) {
	case Copy:
		sources, options.Parents, options.Excludes = copied.Sources, copied.Parents, copied.Excludes
	case Add:
		if hasSpecialAddSource(copied.Sources) {
			return "", errors.New("remote ADD requires its resolved content identity")
		}
		sources, options.Parents, options.Excludes = copied.Sources, copied.Parents, copied.Excludes
	}
	if len(sources) == 0 {
		// Inline bytes and paths are already part of the operation key.
		return digest.FromString("coopr.captured-copy-input.v1/inline"), nil
	}
	if capture.index == nil || capture.directory == "" || policy.directory != capture.directory {
		return "", errors.New("COPY/ADD selection requires its owned capture and policy")
	}
	options, sources, err = policy.applyLocalCopy(options, sources)
	if err != nil {
		return "", err
	}
	// Match native Add's source discovery, without archive sniffing or reading
	// payloads. Keeping excluded descendants is conservative: options stay keyed.
	stats, err := copier.Stat(capture.directory, capture.directory, copier.StatOptions{}, sources)
	if err != nil {
		return "", fmt.Errorf("select captured COPY/ADD sources: %w", err)
	}
	var paths [][2][]byte
	var selected []string
	for _, glob := range stats {
		if glob.Error != "" {
			return "", fmt.Errorf("select captured COPY/ADD source: %s", glob.Error)
		}
		if len(glob.Globbed) == 0 {
			return "", fmt.Errorf("captured COPY/ADD source %q matched nothing", glob.Glob)
		}
		for _, match := range glob.Globbed {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if item := glob.Results[match]; item.Error != "" {
				return "", fmt.Errorf("select captured COPY/ADD source %q: %s", match, item.Error)
			}
			name := match
			if filepath.IsAbs(name) {
				name, err = filepath.Rel(capture.directory, name)
				if err != nil {
					return "", err
				}
			}
			if !filepath.IsLocal(name) {
				return "", fmt.Errorf("captured COPY/ADD source %q escapes its capture", match)
			}
			resolved, err := copier.Eval(capture.directory, filepath.Join(capture.directory, name), copier.EvalOptions{})
			if err != nil {
				return "", fmt.Errorf("resolve captured COPY/ADD source %q: %w", match, err)
			}
			relative, err := filepath.Rel(capture.directory, resolved)
			if err != nil || !filepath.IsLocal(relative) {
				return "", fmt.Errorf("resolved COPY/ADD source %q escapes its capture", match)
			}
			// Native excludes also see the resolved source path. Preserve both
			// names so retargeting an otherwise identical symlink cannot alias it.
			// Byte slices preserve non-UTF-8 filesystem names in the JSON framing.
			paths = append(paths, [2][]byte{[]byte(filepath.ToSlash(name)), []byte(filepath.ToSlash(relative))})
			selected = append(selected, filepath.ToSlash(relative))
		}
	}
	if options.Parents || capture.index.HasNonUTF8Paths() {
		// Native --parents also copies ancestor metadata. The full indexed view
		// covers it without another walk. It also retains raw binary filenames
		// that cannot round-trip through native copier's JSON metadata.
		selected = []string{"."}
	}
	view, err := capture.index.SelectCopy(ctx, selected)
	if err != nil {
		return "", err
	}
	document, err := json.Marshal(struct {
		Domain string        `json:"domain"`
		Paths  [][2][]byte   `json:"paths"`
		View   digest.Digest `json:"view"`
	}{"coopr.captured-copy-input.v1", paths, view})
	if err != nil {
		return "", err
	}
	return digest.FromBytes(document), nil
}
