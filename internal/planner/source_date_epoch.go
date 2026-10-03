package planner

import (
	"fmt"
	"maps"
	"strconv"
	"strings"

	"coopr/internal/buildcontext"
	"coopr/internal/definition"
)

const maxSourceDateEpoch = int64(253402300799) // 9999-12-31T23:59:59Z

type SourceDateEpochSourceKind string

const (
	SourceDateEpochNamedContext SourceDateEpochSourceKind = "named-context"
	SourceDateEpochHTTP         SourceDateEpochSourceKind = "http"
	SourceDateEpochGit          SourceDateEpochSourceKind = "git"
)

type SourceDateEpochSource struct {
	Kind      SourceDateEpochSourceKind
	Name      string
	Reference string
	Checksum  string
	Context   *buildcontext.Spec
}

type SourceDateEpochResolver func(SourceDateEpochSource) (*int64, error)

func parseNumericSourceDateEpoch(value string) (*int64, string, bool, error) {
	if value == "" {
		return nil, "", true, nil
	}
	epoch, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil, "", false, nil
	}
	if epoch < 0 || epoch > maxSourceDateEpoch {
		return nil, "", true, fmt.Errorf("SOURCE_DATE_EPOCH must be empty or a base-10 Unix timestamp from 0 through %d, got %q", maxSourceDateEpoch, value)
	}
	canonical := strconv.FormatInt(epoch, 10)
	return &epoch, canonical, true, nil
}

func (g *graph) resolveSourceDateEpoch(value string) (*int64, string, error) {
	if epoch, canonical, numeric, err := parseNumericSourceDateEpoch(value); numeric {
		return epoch, canonical, err
	}
	if value == "context" {
		return validateResolvedSourceDateEpoch(g.opts.ContextSourceDateEpoch, "primary context")
	}
	if context, found := g.contexts[canonicalStageName(value)]; found {
		return g.resolveSourceDateEpochSource(SourceDateEpochSource{Kind: SourceDateEpochNamedContext, Name: context.Name, Reference: context.Reference, Context: cloneContextSpec(context)})
	}
	if stage, found := g.aliases[canonicalStageName(value)]; found {
		source, err := g.sourceDateEpochStageSource(stage)
		if err != nil {
			return nil, "", err
		}
		return g.resolveSourceDateEpochSource(source)
	}
	return nil, "", fmt.Errorf("invalid SOURCE_DATE_EPOCH: %s", value)
}

func (g *graph) resolveSourceDateEpochSource(source SourceDateEpochSource) (*int64, string, error) {
	if g.opts.SourceDateEpochResolver == nil {
		return nil, "", nil
	}
	epoch, err := g.opts.SourceDateEpochResolver(source)
	if err != nil {
		return nil, "", fmt.Errorf("resolve SOURCE_DATE_EPOCH %s %q: %w", source.Kind, source.Name, err)
	}
	return validateResolvedSourceDateEpoch(epoch, fmt.Sprintf("SOURCE_DATE_EPOCH %s %q", source.Kind, source.Name))
}

func validateResolvedSourceDateEpoch(epoch *int64, description string) (*int64, string, error) {
	if epoch == nil {
		return nil, "", nil
	}
	if *epoch < 0 || *epoch > maxSourceDateEpoch {
		return nil, "", fmt.Errorf("%s timestamp must be from 0 through %d, got %d", description, maxSourceDateEpoch, *epoch)
	}
	value := *epoch
	return &value, strconv.FormatInt(value, 10), nil
}

func (g *graph) sourceDateEpochStageSource(index int) (SourceDateEpochSource, error) {
	stage := g.raw[index]
	if stage.head.Name != "from" {
		return SourceDateEpochSource{}, fmt.Errorf("SOURCE_DATE_EPOCH stage must use FROM scratch")
	}
	values := maps.Clone(g.globals)
	delete(values, "SOURCE_DATE_EPOCH")
	base, err := expand(stage.head.Arguments[0], g.expansionScope(values))
	if err != nil {
		return SourceDateEpochSource{}, fmt.Errorf("SOURCE_DATE_EPOCH stage base: %w", err)
	}
	if !strings.EqualFold(base, "scratch") {
		return SourceDateEpochSource{}, fmt.Errorf("SOURCE_DATE_EPOCH stage must use FROM scratch")
	}

	var source *SourceDateEpochSource
	for _, inst := range stage.body {
		switch inst.Name {
		case "arg":
			if err := g.applySourceDateEpochStageArg(inst, values); err != nil {
				return SourceDateEpochSource{}, err
			}
		case "add":
			if source != nil {
				return SourceDateEpochSource{}, fmt.Errorf("SOURCE_DATE_EPOCH stage must contain exactly one remote ADD")
			}
			resolved, err := sourceDateEpochAddSource(inst, g.expansionScope(values))
			if err != nil {
				return SourceDateEpochSource{}, err
			}
			source = &resolved
		default:
			return SourceDateEpochSource{}, fmt.Errorf("SOURCE_DATE_EPOCH stage does not meet source-only requirements: unsupported %s instruction", inst.Name)
		}
	}
	if source == nil {
		return SourceDateEpochSource{}, fmt.Errorf("SOURCE_DATE_EPOCH stage must contain exactly one remote ADD")
	}
	return *source, nil
}

func (g *graph) applySourceDateEpochStageArg(inst definition.Instruction, values scope) error {
	name := inst.Arguments[0]
	if name == "SOURCE_DATE_EPOCH" {
		return nil
	}
	value := values[name]
	if supplied, ok := g.opts.Arguments[name]; ok {
		value = binding{value: supplied, present: true}
	} else if len(inst.Arguments) == 2 {
		expanded, err := expand(inst.Arguments[1], g.expansionScope(values))
		if err != nil {
			return fmt.Errorf("SOURCE_DATE_EPOCH stage argument %q: %w", name, err)
		}
		value = binding{value: expanded, present: true}
	}
	values[name] = value
	return nil
}

func sourceDateEpochAddSource(inst definition.Instruction, values scope) (SourceDateEpochSource, error) {
	if len(inst.InlineFiles) != 0 || len(inst.Arguments) != 2 {
		return SourceDateEpochSource{}, fmt.Errorf("SOURCE_DATE_EPOCH stage must contain exactly one remote ADD source")
	}
	reference, err := expand(inst.Arguments[0], values)
	if err != nil {
		return SourceDateEpochSource{}, fmt.Errorf("SOURCE_DATE_EPOCH stage ADD source: %w", err)
	}
	checksum, err := expand(inst.Properties["checksum"], values)
	if err != nil {
		return SourceDateEpochSource{}, fmt.Errorf("SOURCE_DATE_EPOCH stage ADD checksum: %w", err)
	}
	source := SourceDateEpochSource{Name: reference, Reference: reference, Checksum: checksum}
	switch {
	case isGitSourceDateEpochReference(reference):
		source.Kind = SourceDateEpochGit
	case isHTTPSourceDateEpochReference(reference):
		source.Kind = SourceDateEpochHTTP
	default:
		return SourceDateEpochSource{}, fmt.Errorf("SOURCE_DATE_EPOCH stage source must be a single HTTP(S) or Git ADD")
	}
	return source, nil
}

func isHTTPSourceDateEpochReference(value string) bool {
	return strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")
}

func isGitSourceDateEpochReference(value string) bool {
	withoutFragment, _, _ := strings.Cut(value, "#")
	return strings.HasPrefix(withoutFragment, "git://") || strings.HasPrefix(withoutFragment, "ssh://") || strings.HasPrefix(withoutFragment, "git@") ||
		(isHTTPSourceDateEpochReference(withoutFragment) && strings.HasSuffix(withoutFragment, ".git"))
}
