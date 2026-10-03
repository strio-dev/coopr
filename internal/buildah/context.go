package buildah

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/copier"
	buildahparse "go.podman.io/buildah/pkg/parse"
	"go.podman.io/storage/pkg/fileutils"
)

// contextPolicy combines the authored context policy with executor-owned paths.
// Protected paths are deliberately applied last so an authored negation cannot
// make an active Coopr output, cache, or store visible to COPY or ADD.
type contextPolicy struct {
	directory         string
	ignoreFile        string
	authoredExcludes  []string
	protectedExcludes []string
}

func prepareContextPolicy(contextDir string, artifacts []string) (contextPolicy, error) {
	return prepareContextPolicyWithIgnore(contextDir, artifacts, "")
}

func prepareContextPolicyWithIgnore(contextDir string, artifacts []string, ignoreFile string) (contextPolicy, error) {
	if contextDir == "" {
		if len(artifacts) != 0 {
			return contextPolicy{}, errors.New("context artifacts require a build context")
		}
		return contextPolicy{}, nil
	}
	contextRoot, err := filepath.Abs(contextDir)
	if err != nil {
		return contextPolicy{}, fmt.Errorf("resolve build context: %w", err)
	}
	canonicalRoot, err := canonicalStorePath(contextRoot)
	if err != nil {
		return contextPolicy{}, fmt.Errorf("resolve build context: %w", err)
	}

	authored, ignoreFile, err := ReadContextIgnore(contextRoot, ignoreFile)
	if err != nil {
		return contextPolicy{}, err
	}
	protected, err := protectedContextExcludes(contextRoot, canonicalRoot, artifacts)
	if err != nil {
		return contextPolicy{}, err
	}
	return contextPolicy{
		directory: contextRoot, ignoreFile: ignoreFile,
		authoredExcludes: authored, protectedExcludes: protected,
	}, nil
}

func (policy contextPolicy) apply(options upstream.AddAndCopyOptions) (upstream.AddAndCopyOptions, error) {
	instructionExcludes := slices.Clone(options.Excludes)
	if _, err := fileutils.NewPatternMatcher(instructionExcludes); err != nil {
		return upstream.AddAndCopyOptions{}, fmt.Errorf("validate COPY/ADD excludes: %w", err)
	}
	options.ContextDir = policy.directory
	options.IgnoreFile = policy.ignoreFile
	if len(policy.authoredExcludes) != 0 && hasNegatedExclude(instructionExcludes) {
		independent, err := independentContextExcludes(policy.directory, policy.authoredExcludes, instructionExcludes)
		if err != nil {
			return upstream.AddAndCopyOptions{}, err
		}
		options.Excludes = independent
	} else {
		options.Excludes = append(slices.Clone(policy.authoredExcludes), instructionExcludes...)
	}
	options.Excludes = append(options.Excludes, policy.protectedExcludes...)
	if _, err := fileutils.NewPatternMatcher(options.Excludes); err != nil {
		return upstream.AddAndCopyOptions{}, fmt.Errorf("validate context excludes: %w", err)
	}
	return options, nil
}

// applyLocalCopy matches the policy used by Buildah's Containerfile frontend
// for COPY and local ADD sources.  Instruction excludes are interpreted both
// from the context root and relative to each source, while set-ID bits are
// stripped only for context sources (COPY --from preserves source metadata).
func (policy contextPolicy) applyLocalCopy(options upstream.AddAndCopyOptions, sources []string) (upstream.AddAndCopyOptions, []string, error) {
	options.Excludes = copySourceExcludes(options.Excludes, sources)
	options.StripSetuidBit = true
	options.StripSetgidBit = true
	applied, err := policy.apply(options)
	if err != nil {
		return upstream.AddAndCopyOptions{}, nil, err
	}
	return applied, preserveParentsPivot(policy.directory, options.Parents, sources), nil
}

func copySourceExcludes(excludes, sources []string) []string {
	result := slices.Clone(excludes)
	if len(excludes) == 0 {
		return result
	}
	for _, source := range sources {
		normalized := strings.TrimLeft(filepath.ToSlash(filepath.Clean(source)), "/")
		if normalized == "" {
			normalized = "."
		}
		for _, exclude := range excludes {
			negated := strings.HasPrefix(exclude, "!")
			pattern := strings.TrimPrefix(exclude, "!")
			if pattern == "" || normalized == "." {
				result = append(result, exclude)
				continue
			}
			pattern = path.Join(normalized, pattern)
			if negated {
				pattern = "!" + pattern
			}
			result = append(result, pattern)
		}
	}
	return result
}

func preserveParentsPivot(contextDir string, parents bool, sources []string) []string {
	result := slices.Clone(sources)
	if !parents {
		return result
	}
	for index, source := range result {
		_, suffix, found := strings.Cut(filepath.ToSlash(source), "/./")
		if !found {
			continue
		}
		fullPath := filepath.Join(contextDir, filepath.FromSlash(source))
		suffix = filepath.Clean(filepath.FromSlash(suffix))
		prefix := filepath.Clean(strings.TrimSuffix(fullPath, suffix))
		result[index] = filepath.ToSlash(prefix) + "/./" + filepath.ToSlash(suffix)
	}
	return result
}

func hasNegatedExclude(patterns []string) bool {
	for _, pattern := range patterns {
		if strings.HasPrefix(strings.TrimSpace(pattern), "!") {
			return true
		}
	}
	return false
}

// independentContextExcludes reduces two ordered pattern lists to one matcher
// over the current context tree. A path is excluded when either layer excludes
// it, so a COPY/ADD --exclude negation can only reopen an earlier --exclude
// pattern and can never reopen a path removed by .dockerignore.
func independentContextExcludes(contextRoot string, layers ...[]string) ([]string, error) {
	matchers := make([]*fileutils.PatternMatcher, 0, len(layers))
	for _, patterns := range layers {
		matcher, err := fileutils.NewPatternMatcher(patterns)
		if err != nil {
			return nil, fmt.Errorf("validate context excludes: %w", err)
		}
		matchers = append(matchers, matcher)
	}

	result := []string{}
	directoryStates := map[string]bool{".": false}
	err := filepath.WalkDir(contextRoot, func(candidate string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(contextRoot, candidate)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		excluded := false
		for _, matcher := range matchers {
			matched, err := matcher.IsMatch(filepath.ToSlash(relative))
			if err != nil {
				return err
			}
			excluded = excluded || matched
		}
		parentExcluded := directoryStates[filepath.Dir(relative)]
		if excluded != parentExcluded {
			pattern := quoteExcludePath(relative)
			if !excluded {
				pattern = "!" + pattern
			}
			result = append(result, pattern)
		}
		if entry.IsDir() {
			directoryStates[relative] = excluded
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inspect build context for independent excludes: %w", err)
	}
	return result, nil
}

// ReadContextIgnore selects one context-root policy: .cooprignore, then
// .containerignore, then .dockerignore. An explicit file replaces discovery.
// Default paths follow Buildah's confined symlink resolution.
func ReadContextIgnore(contextRoot, ignoreFile string) ([]string, string, error) {
	if ignoreFile == "" {
		cooprIgnore, err := copier.Eval(contextRoot, ".cooprignore", copier.EvalOptions{})
		if err != nil {
			return nil, "", fmt.Errorf("resolve coopr ignore policy: %w", err)
		}
		patterns, selected, err := buildahparse.ContainerIgnoreFile(contextRoot, cooprIgnore, nil)
		if !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return nil, "", fmt.Errorf("read coopr ignore policy: %w", err)
			}
			return patterns, selected, nil
		}
	}
	patterns, selected, err := buildahparse.ContainerIgnoreFile(contextRoot, ignoreFile, nil)
	if err != nil {
		return nil, "", fmt.Errorf("read container ignore policy: %w", err)
	}
	return patterns, selected, nil
}

func protectedContextExcludes(contextRoot, canonicalRoot string, artifacts []string) ([]string, error) {
	protected := []string{}
	infos := []os.FileInfo{}
	for _, artifact := range artifacts {
		if artifact == "" {
			continue
		}
		absolute, err := filepath.Abs(artifact)
		if err != nil {
			return nil, fmt.Errorf("resolve context artifact %q: %w", artifact, err)
		}
		canonical, err := canonicalStorePath(absolute)
		if err != nil {
			return nil, fmt.Errorf("resolve context artifact %q: %w", artifact, err)
		}
		if pathContains(absolute, contextRoot) || pathContains(canonical, canonicalRoot) {
			return nil, fmt.Errorf("context artifact %q covers build context %q", artifact, contextRoot)
		}
		for _, pair := range [][2]string{{contextRoot, absolute}, {canonicalRoot, canonical}} {
			if pathContains(pair[0], pair[1]) {
				relative, err := filepath.Rel(pair[0], pair[1])
				if err != nil {
					return nil, fmt.Errorf("locate context artifact %q: %w", artifact, err)
				}
				protected = appendUnique(protected, quoteExcludePath(relative))
			}
		}
		if info, err := os.Stat(absolute); err == nil {
			infos = append(infos, info)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect context artifact %q: %w", artifact, err)
		}
	}
	if len(infos) == 0 {
		return protected, nil
	}
	err := filepath.WalkDir(contextRoot, func(candidate string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if candidate == contextRoot {
			return nil
		}
		info, err := os.Stat(candidate)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		for _, artifactInfo := range infos {
			if !os.SameFile(info, artifactInfo) {
				continue
			}
			relative, err := filepath.Rel(contextRoot, candidate)
			if err != nil {
				return err
			}
			protected = appendUnique(protected, quoteExcludePath(relative))
			if entry.IsDir() {
				return filepath.SkipDir
			}
			break
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inspect build context for artifact aliases: %w", err)
	}
	return protected, nil
}

func quoteExcludePath(value string) string {
	value = filepath.ToSlash(filepath.Clean(value))
	var quoted strings.Builder
	for _, character := range value {
		if strings.ContainsRune(`\*?[]!`, character) {
			quoted.WriteByte('\\')
		}
		quoted.WriteRune(character)
	}
	return quoted.String()
}

func appendUnique(values []string, value string) []string {
	if value == "" || value == "." || slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}
