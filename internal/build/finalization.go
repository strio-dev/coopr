package build

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"coopr/internal/buildah"
	"coopr/internal/oci"
	"coopr/internal/transfer"
)

func validateFinalization(opts *Options, platforms []string, destinations []transfer.Destination) error {
	outputs := filesystemOutputs(opts.Output, opts.Outputs)
	stdout := false
	for i := range outputs {
		output := &outputs[i]
		if output.Path == "" {
			continue
		}
		if output.Type != "local" && output.Type != "tar" {
			return errors.New("filesystem output must be local or tar")
		}
		if output.Path == "-" && output.Type != "tar" {
			return errors.New("stdout filesystem output requires tar")
		}
		if output.Path == "-" {
			if stdout {
				return errors.New("only one filesystem output may use stdout")
			}
			stdout = true
		}
		if output.Path != "-" {
			path, err := filepath.Abs(output.Path)
			if err != nil {
				return err
			}
			output.Path = path
		}
	}
	opts.Output = buildah.FilesystemOutput{}
	opts.Outputs = outputs
	opts.SBOM = slices.Clone(opts.SBOM)
	for i := range opts.SBOM {
		scan := &opts.SBOM[i]
		if scan.Image == "" || len(scan.Commands) == 0 {
			return errors.New("SBOM scan requires a scanner image and command")
		}
		for _, path := range []*string{&scan.SBOMOutput, &scan.PURLOutput} {
			if *path != "" {
				absolute, err := filepath.Abs(*path)
				if err != nil {
					return err
				}
				*path = absolute
			}
		}
	}
	if opts.Signing.Enabled() {
		probe := transfer.Destination{Transport: "local"}
		if opts.Signing.SigstorePrivateKeyFile != "" {
			probe.Transport = "registry"
		}
		if err := transfer.ValidateSigningDestination(oci.Image, probe, opts.Signing); err != nil {
			return err
		}
		validDestination := false
		for _, destination := range destinations {
			if opts.Signing.SigstorePrivateKeyFile != "" && destination.Transport == "registry" || opts.Signing.SignBy != "" && (destination.Transport == "registry" || destination.Transport == "local") {
				validDestination = true
			}
		}
		if !validDestination {
			if opts.Signing.SigstorePrivateKeyFile != "" {
				return errors.New("sigstore image signing requires a registry destination (--tag registry:NAME or --push --tag NAME)")
			}
			return errors.New("GPG image signing requires a local or registry destination (--tag NAME or --tag registry:NAME)")
		}
	} else if opts.Signing.PassphraseFile != "" {
		return errors.New("--sign-passphrase-file requires --sign-by or --sign-by-sigstore-private-key")
	}
	return nil
}

func finalizationArtifacts(opts Options, artifacts []string) ([]string, error) {
	paths := []string{}
	for _, output := range filesystemOutputs(opts.Output, opts.Outputs) {
		if output.Path != "" && output.Path != "-" {
			paths = append(paths, output.Path)
		}
	}
	for _, scan := range opts.SBOM {
		for _, path := range []string{scan.SBOMOutput, scan.PURLOutput} {
			if path != "" {
				paths = append(paths, path)
			}
		}
	}
	allPaths := append(slices.Clone(artifacts), paths...)
	signingInputs, err := opts.Signing.ContextArtifacts()
	if err != nil {
		return nil, err
	}
	inputs := append([]string{opts.File}, signingInputs...)
	if err := validateArtifactOverlaps(allPaths, inputs); err != nil {
		return nil, err
	}
	return paths, nil
}

func validateArtifactOverlaps(paths, inputs []string) error {
	for i, path := range paths {
		for _, input := range inputs {
			if input == "" || input == "-" || isHTTPDefinition(input) {
				continue
			}
			if overlaps, err := pathsOverlap(path, input); err != nil {
				return err
			} else if overlaps {
				return fmt.Errorf("output %s overlaps input %s", path, definitionDisplayName(input))
			}
		}
		for _, other := range paths[:i] {
			if overlaps, err := pathsOverlap(path, other); err != nil {
				return err
			} else if overlaps {
				return fmt.Errorf("output destinations overlap: %s and %s", path, other)
			}
		}
	}
	return nil
}

func pathsOverlap(a, b string) (bool, error) {
	if same, err := sameDestination(a, b); err != nil || same {
		return same, err
	}
	left, err := canonicalParentPath(a)
	if err != nil {
		return false, err
	}
	right, err := canonicalParentPath(b)
	if err != nil {
		return false, err
	}
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		left = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		right = resolved
	}
	return pathContains(left, right) || pathContains(right, left), nil
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func preflightFilesystemOutput(output buildah.FilesystemOutput, definition string, stores ...string) error {
	if output.Path == "" || output.Path == "-" || output.Type != "local" {
		return nil
	}
	for _, store := range stores {
		if overlaps, err := pathsOverlap(output.Path, store); err != nil {
			return err
		} else if overlaps {
			return fmt.Errorf("filesystem output %s overlaps Coopr store %s", output.Path, store)
		}
	}
	if definition != "-" && !isHTTPDefinition(definition) {
		if overlaps, err := pathsOverlap(output.Path, definition); err != nil {
			return err
		} else if overlaps {
			return fmt.Errorf("filesystem output %s overlaps definition %s", output.Path, definitionDisplayName(definition))
		}
	}
	if info, err := os.Lstat(output.Path); err == nil && !info.IsDir() {
		return fmt.Errorf("local filesystem output %s must be a directory", output.Path)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.MkdirAll(filepath.Dir(output.Path), 0o755)
}

func platformFilesystemOutput(output buildah.FilesystemOutput, platform string, count int) buildah.FilesystemOutput {
	if count > 1 && output.Type == "local" && output.Path != "" {
		output.Path = filepath.Join(output.Path, strings.ReplaceAll(platform, "/", "_"))
	}
	return output
}

func filesystemOutputs(single buildah.FilesystemOutput, outputs []buildah.FilesystemOutput) []buildah.FilesystemOutput {
	result := slices.Clone(outputs)
	if single.Path != "" {
		result = append([]buildah.FilesystemOutput{single}, result...)
	}
	return result
}

func platformFilesystemOutputs(outputs []buildah.FilesystemOutput, platform string, count int) []buildah.FilesystemOutput {
	result := make([]buildah.FilesystemOutput, len(outputs))
	for i := range outputs {
		result[i] = platformFilesystemOutput(outputs[i], platform, count)
	}
	return result
}
