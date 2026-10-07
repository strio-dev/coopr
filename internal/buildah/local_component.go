package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"coopr/internal/buildcontext"
	"coopr/internal/componentstore"
	"coopr/internal/definition"
	"coopr/internal/oci"
	"coopr/internal/planner"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/copier"
	orasoci "oras.land/oras-go/v2/content/oci"
)

// readLocalComponent uses the same confined source reader and context policy
// as COPY. In particular, absolute paths and parent traversal are interpreted
// inside the build context, never as unrestricted host paths.
func readLocalComponent(ctx context.Context, options PlanOptions, reference string) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if options.ContextDir == "" {
		return nil, "", errors.New("local component requires a build context")
	}
	artifacts := append(slices.Clone(options.ContextArtifacts), options.Store.RunRoot, options.Store.GraphRoot, options.Output.Path, options.CacheLocalDir)
	if options.Resolver != nil {
		artifacts = append(artifacts, options.Resolver.ComponentStoreDir())
	}
	policy, err := prepareContextPolicyWithIgnore(options.ContextDir, artifacts, options.IgnoreFile)
	if err != nil {
		return nil, "", err
	}
	copyOptions, _, err := policy.applyLocalCopy(upstream.AddAndCopyOptions{}, []string{reference})
	if err != nil {
		return nil, "", err
	}
	stats, err := copier.Stat(policy.directory, policy.directory, copier.StatOptions{Excludes: copyOptions.Excludes}, []string{reference})
	if err != nil {
		return nil, "", err
	}
	if len(stats) != 1 || stats[0].Error != "" || len(stats[0].Globbed) != 1 {
		return nil, "", fmt.Errorf("local component %q must select exactly one definition file", reference)
	}
	name := stats[0].Globbed[0]
	item := stats[0].Results[name]
	if item == nil || item.Error != "" || !item.IsRegular {
		return nil, "", fmt.Errorf("local component %q must be a regular definition file", reference)
	}
	canonical, err := copier.Eval(policy.directory, name, copier.EvalOptions{})
	if err != nil {
		return nil, "", err
	}
	reader, writer := io.Pipe()
	result := make(chan error, 1)
	go func() {
		err := copier.Get(policy.directory, policy.directory, copier.GetOptions{Excludes: copyOptions.Excludes}, []string{reference}, writer)
		result <- errors.Join(err, writer.CloseWithError(err))
	}()
	data, readErr := readComponentArchive(ctx, reader)
	_ = reader.CloseWithError(readErr)
	if err := errors.Join(readErr, <-result); err != nil {
		return nil, "", fmt.Errorf("read local component %q: %w", reference, err)
	}
	return data, canonical, nil
}

func readComponentArchive(ctx context.Context, reader io.Reader) ([]byte, error) {
	archive := tar.NewReader(reader)
	var data []byte
	selected := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if selected || header.Typeflag != tar.TypeReg {
			return nil, errors.New("local component source must contain exactly one regular file")
		}
		data, err = io.ReadAll(archive)
		if err != nil {
			return nil, err
		}
		selected = true
	}
	if !selected {
		return nil, errors.New("local component source was excluded or missing")
	}
	return data, nil
}

func (executor *graphExecutor) materializeLocalComponent(ctx context.Context, reference string, parameters map[string]string, target v1.Platform, progressPrefix string) (_ string, _ string, retErr error) {
	if executor.options.Resolver == nil {
		return "", "", errors.New("local component requires a component resolver")
	}
	source, canonical, err := readLocalComponent(ctx, executor.options, reference)
	if err != nil {
		return "", "", err
	}
	// COPY's canonical source identity also catches symlink and traversal aliases.
	// Identical definitions at different paths may form a finite parameterized DAG.
	identity := "path:" + canonical
	release, err := executor.enterLocalComponent(identity)
	if err != nil {
		return "", "", err
	}
	defer release()
	def, err := definition.Parse(bytes.NewReader(source))
	if err != nil {
		return "", "", fmt.Errorf("parse local component %q: %w", reference, err)
	}
	platform, err := componentPlatformString(target)
	if err != nil {
		return "", "", err
	}
	plan, selected, err := planDefinitionWithExternalBaseState(ctx, def, planner.Options{
		Mode: planner.Publish, Platform: platform, Arguments: maps.Clone(parameters),
		SourceDateEpochResolver: sourceDateEpochResolver(ctx, buildCredentialSource{secretSpecs: executor.options.Secrets, sshSpecs: executor.options.SSH}),
	}, executor.resolveBaseImage, func(ctx context.Context, spec buildcontext.Spec, target v1.Platform) (ResolvedImageSource, error) {
		return MaterializeNamedContext(ctx, spec, target, executor.store, platformSystemContext(executor.system, target), executor.options.Resolver, executor.options.Secrets, executor.options.SSH, executor.options.ContextArtifacts...)
	})
	if err != nil {
		return "", "", fmt.Errorf("plan local component %q: %w", reference, err)
	}
	workspace, err := os.MkdirTemp("", "coopr-local-component-*")
	if err != nil {
		return "", "", err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(workspace)) }()
	paths := publicationPackagePaths(plan, workspace)
	options := executor.options
	options.ProgressPrefix = progressPrefix + "[package " + progressText(reference) + "] "
	options.ProgressReference = ""
	options.progressStageTotal = definitionStageCount(def)
	options.componentParent = executor
	options.ContextArtifacts = append(slices.Clone(options.ContextArtifacts), workspace, executor.options.Output.Path)
	options.ResolvedBases = maps.Clone(executor.resolvedBases)
	maps.Copy(options.ResolvedBases, selected.all)
	options.ReplannedBaseDelta = selected.takeDelta
	packages, err := PublishPlan(ctx, plan, PublicationOptions{PlanOptions: options, PackagePaths: paths})
	if err != nil {
		return "", "", fmt.Errorf("build local component %q: %w", reference, err)
	}
	names := make([]string, 0, len(packages))
	for name := range packages {
		names = append(names, name)
	}
	slices.Sort(names)
	ordered := make([]oci.Package, 0, len(names))
	for _, name := range names {
		ordered = append(ordered, packages[name])
	}
	layout := filepath.Join(workspace, "layout")
	root, err := oci.WriteComponentLayout(ctx, layout, oci.ComponentMetadata{
		Version: oci.ComponentVersion, Platform: target, Component: *plan.Component, Packages: ordered,
	}, paths)
	if err != nil {
		return "", "", fmt.Errorf("store local component %q: %w", reference, err)
	}
	sourceStore, err := orasoci.NewWithContext(ctx, layout)
	if err != nil {
		return "", "", err
	}
	if err := componentstore.Put(ctx, executor.options.Resolver.ComponentStoreDir(), sourceStore, root, ""); err != nil {
		return "", "", fmt.Errorf("install local component %q: %w", reference, err)
	}
	return root.Digest.String(), identity, nil
}

func (executor *graphExecutor) enterLocalComponent(identity string) (func(), error) {
	if identity == "" {
		return func() {}, nil
	}
	if executor.activeLocalComponents[identity] {
		return nil, fmt.Errorf("local component invocation cycle at %s", identity)
	}
	if executor.activeLocalComponents == nil {
		executor.activeLocalComponents = make(map[string]bool)
	}
	executor.activeLocalComponents[identity] = true
	return func() { delete(executor.activeLocalComponents, identity) }, nil
}
