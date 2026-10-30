package buildah

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.podman.io/buildah/copier"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/transports/alltransports"
	"go.podman.io/image/v5/types"
)

// Resolve policy identity and copied bytes through the same context root.
// An absolute symlink inside a context refers to that context, not the host.
func confinedSourcePolicyReference(reference, contextDir string) (types.ImageReference, string, error) {
	transport, value, _ := strings.Cut(reference, ":")
	source, selector, tagged := strings.Cut(value, ":")
	if transport == "dir" {
		source, tagged = value, false
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return nil, "", err
	}
	root := filepath.Dir(source)
	if contextDir != "" {
		root, err = filepath.Abs(contextDir)
		if err != nil {
			return nil, "", err
		}
	}
	relative, err := filepath.Rel(root, source)
	if err != nil {
		return nil, "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, "", err
	}
	resolved, err := copier.Eval(root, relative, copier.EvalOptions{})
	if err != nil {
		return nil, "", err
	}
	value = transport + ":" + resolved
	if tagged {
		value += ":" + selector
	}
	policyReference, err := alltransports.ParseImageName(value)
	if err != nil {
		return nil, "", err
	}
	if identity := policyReference.PolicyConfigurationIdentity(); identity != "" && identity != resolved {
		return nil, "", fmt.Errorf("image source identity changed during confined path resolution: %q", resolved)
	}
	return policyReference, root, nil
}

// snapshotSourcePolicy keeps the authored source's policy requirements while
// native image readers use a confined snapshot. Wrapping ImageSource would hide
// its private signature-format methods and break native Sigstore verification.
func snapshotSourcePolicy(system *types.SystemContext, original types.ImageReference, snapshot string) (string, func() error, error) {
	frozen, err := alltransports.ParseImageName(snapshot)
	if err != nil {
		return "", nil, err
	}
	if original.Transport().Name() != frozen.Transport().Name() {
		return "", nil, errors.New("snapshot changed image source transport")
	}
	dockerIdentity := func(reference types.ImageReference) string {
		if named := reference.DockerReference(); named != nil {
			return named.String()
		}
		return ""
	}
	if dockerIdentity(original) != dockerIdentity(frozen) {
		return "", nil, errors.New("snapshot changed image source Docker identity")
	}
	identity := frozen.PolicyConfigurationIdentity()
	if original.PolicyConfigurationIdentity() == identity && slices.Equal(original.PolicyConfigurationNamespaces(), frozen.PolicyConfigurationNamespaces()) {
		return system.SignaturePolicyPath, nil, nil
	}
	policy, err := signature.DefaultPolicy(system)
	if err != nil {
		return "", nil, err
	}
	requirements := sourcePolicyRequirements(policy, original)
	transport := frozen.Transport().Name()
	bound := &signature.Policy{
		Default: signature.PolicyRequirements{signature.NewPRReject()},
		Transports: map[string]signature.PolicyTransportScopes{
			transport: {identity: requirements},
		},
	}
	data, err := json.Marshal(bound)
	if err != nil {
		return "", nil, err
	}
	file, err := os.CreateTemp("", "coopr-source-policy-*.json")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() error { return os.Remove(file.Name()) }
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return "", nil, errors.Join(err, cleanup())
	}
	return file.Name(), cleanup, nil
}

// PolicyContext's requirement selector is private. Preserve its exact scope
// precedence (containers/image v5.41.2 signature/policy_eval.go) while delegating
// requirement parsing, signature verification and image copying to upstream.
func sourcePolicyRequirements(policy *signature.Policy, reference types.ImageReference) signature.PolicyRequirements {
	scopes := policy.Transports[reference.Transport().Name()]
	if requirements, ok := scopes[reference.PolicyConfigurationIdentity()]; ok {
		return requirements
	}
	for _, namespace := range reference.PolicyConfigurationNamespaces() {
		if requirements, ok := scopes[namespace]; ok {
			return requirements
		}
	}
	if requirements, ok := scopes[""]; ok {
		return requirements
	}
	return policy.Default
}
