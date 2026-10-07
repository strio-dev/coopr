package buildah

import (
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"coopr/internal/planner"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/runtime-spec/specs-go"
	"go.podman.io/buildah/define"
	buildahparse "go.podman.io/buildah/pkg/parse"
)

const defaultBuildNetwork = "default"

// NormalizeBuildNetworkOptions validates and canonicalizes build-wide RUN
// networking before planning or worker serialization.
func NormalizeBuildNetworkOptions(network string, addHosts []string) (string, []string, error) {
	network, err := normalizeBuildNetwork(network)
	if err != nil {
		return "", nil, err
	}
	addHosts, err = normalizeAddHosts(addHosts)
	if err != nil {
		return "", nil, err
	}
	return network, addHosts, nil
}

func normalizeBuildNetwork(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultBuildNetwork, nil
	}
	switch value {
	case defaultBuildNetwork, "private", "none", "host":
		return value, nil
	}
	if path, found := strings.CutPrefix(value, "ns:"); found {
		if path == "" {
			return "", fmt.Errorf("network namespace path must not be empty")
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve network namespace %q: %w", path, err)
		}
		if _, err := os.Stat(absolute); err != nil {
			return "", fmt.Errorf("inspect network namespace %q: %w", path, err)
		}
		return "ns:" + absolute, nil
	}
	name, options, hasOptions := strings.Cut(value, ":")
	if name == "pasta" || name == "slirp4netns" {
		if hasOptions {
			if options == "" || strings.ContainsAny(options, "\t\r\n\x00") {
				return "", fmt.Errorf("invalid %s network options %q", name, options)
			}
		}
		return value, nil
	}
	if strings.ContainsAny(value, "\t\r\n\x00:/") {
		return "", fmt.Errorf("invalid named build network %q", value)
	}
	return value, nil
}

func runNetworkOptions(value string) (define.NetworkConfigurationPolicy, define.NamespaceOptions, error) {
	value, err := normalizeBuildNetwork(value)
	if err != nil {
		return define.NetworkDefault, nil, err
	}
	policy := define.NetworkEnabled
	var namespaces define.NamespaceOptions
	switch value {
	case defaultBuildNetwork, "private":
		namespaces.AddOrReplace(define.NamespaceOption{Name: string(specs.NetworkNamespace)})
	case "none":
		policy = define.NetworkDisabled
		namespaces.AddOrReplace(define.NamespaceOption{Name: string(specs.NetworkNamespace)})
	case "host":
		namespaces.AddOrReplace(define.NamespaceOption{Name: string(specs.NetworkNamespace), Host: true})
	default:
		path := strings.TrimPrefix(value, "ns:")
		if strings.HasPrefix(value, "ns:") {
			namespaces.AddOrReplace(define.NamespaceOption{Name: string(specs.NetworkNamespace), Path: path})
		} else {
			namespaces.AddOrReplace(define.NamespaceOption{Name: string(specs.NetworkNamespace), Path: value})
		}
	}
	return policy, namespaces, nil
}

func normalizeAddHosts(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := make([]string, len(values))
	for index, value := range values {
		names, address, found := strings.Cut(value, "=")
		if !found {
			names, address, found = strings.Cut(value, ":")
		}
		names = strings.TrimSpace(names)
		address = strings.TrimSpace(address)
		if !found || names == "" || address == "" {
			return nil, fmt.Errorf("invalid add-host %q: expected HOST:IP or HOST=IP", value)
		}
		normalizedNames := strings.Split(names, ";")
		for nameIndex, name := range normalizedNames {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" || strings.ContainsAny(name, " \t\r\n:") {
				return nil, fmt.Errorf("invalid add-host hostname %q", name)
			}
			normalizedNames[nameIndex] = name
		}
		if address != "host-gateway" {
			if strings.HasPrefix(address, "[") && strings.HasSuffix(address, "]") {
				address = strings.TrimSuffix(strings.TrimPrefix(address, "["), "]")
			}
			ip := net.ParseIP(address)
			if ip == nil {
				return nil, fmt.Errorf("invalid add-host IP address %q", address)
			}
			address = ip.String()
		}
		result[index] = strings.Join(normalizedNames, ";") + ":" + address
	}
	return result, nil
}

func normalizePlanOptions(options PlanOptions) (PlanOptions, error) {
	if options.Lifecycle.NoLayers {
		options.NoCache = true
	}
	if err := validateCacheTTL(options.CacheTTL); err != nil {
		return PlanOptions{}, err
	}
	if options.CacheTTL != nil && *options.CacheTTL == 0 {
		options.NoCache = true
	}
	if err := validateTimestampOptions(options.Timestamp, options.SourceDateEpochOverride, options.RewriteTimestamp); err != nil {
		return PlanOptions{}, err
	}
	if err := options.ImageControls.Validate(); err != nil {
		return PlanOptions{}, err
	}
	if err := validateRunControls(options.RunControls); err != nil {
		return PlanOptions{}, err
	}
	if err := validateRunControlHost(options.RunControls); err != nil {
		return PlanOptions{}, err
	}
	network, addHosts, err := NormalizeBuildNetworkOptions(options.Network, options.AddHosts)
	if err != nil {
		return PlanOptions{}, err
	}
	options.Network = network
	options.AddHosts = addHosts
	// Selecting host networking for the whole build explicitly grants its
	// entitlement, including authored RUN requests and component operations.
	if network == "host" && !slices.Contains(options.Allow, networkHostEntitlement) {
		options.Allow = append(slices.Clone(options.Allow), networkHostEntitlement)
	}
	cacheArtifacts, err := cacheArtifactPaths(options)
	if err != nil {
		return PlanOptions{}, err
	}
	for _, artifact := range cacheArtifacts {
		if !slices.Contains(options.ContextArtifacts, artifact) {
			options.ContextArtifacts = append(options.ContextArtifacts, artifact)
		}
	}
	return options, nil
}

func resolveAndValidateBuildNetwork(stages []planner.Stage, network string, controls RunControls, isolation string) ([]planner.Stage, error) {
	if controls.Isolation != "" {
		isolation = controls.Isolation
	}
	selectedIsolation, err := buildahparse.IsolationOption(isolation)
	if err != nil {
		return nil, fmt.Errorf("select Buildah isolation: %w", err)
	}
	resolved := resolveBuildNetwork(stages, network)
	for _, stage := range resolved {
		for operationIndex, operation := range stage.Operations {
			if operation.Name != "run" {
				continue
			}
			if err := validateRunControlsForNetwork(controls, operation.Properties["network"]); err != nil {
				return nil, fmt.Errorf("stage %s operation %d: %w", stage.ID, operationIndex+1, err)
			}
			if err := validateRunNetworkIsolation(operation.Properties["network"], selectedIsolation); err != nil {
				return nil, fmt.Errorf("stage %s operation %d: %w", stage.ID, operationIndex+1, err)
			}
		}
	}
	return resolved, nil
}

func validateRunNetworkIsolation(network string, isolation define.Isolation) error {
	// Buildah's CLI rejects explicit non-host networking with chroot. Its
	// library API otherwise ignores the requested namespace and runs on host.
	if isolation == define.IsolationChroot && network != "" && network != defaultBuildNetwork && network != "host" {
		return fmt.Errorf("RUN network=%s cannot be used with chroot isolation; use --isolation=oci or --isolation=rootless", network)
	}
	return nil
}

func resolveBuildNetwork(stages []planner.Stage, network string) []planner.Stage {
	result := slices.Clone(stages)
	for stageIndex := range result {
		result[stageIndex].Operations = resolveOperationNetwork(result[stageIndex].Operations, network)
	}
	return result
}

func resolveOperationNetwork(operations []planner.Operation, network string) []planner.Operation {
	result := slices.Clone(operations)
	for index := range result {
		if result[index].Name != "run" || result[index].NetworkExplicit {
			continue
		}
		result[index].Properties = cloneStringMap(result[index].Properties)
		result[index].Properties["network"] = network
	}
	return result
}

func buildExecutionOptionsDigest(stages []planner.Stage, addHosts []string) (digest.Digest, error) {
	type runNetwork struct {
		Stage     string `json:"stage"`
		Operation int    `json:"operation"`
		Network   string `json:"network"`
	}
	networks := make([]runNetwork, 0)
	for _, stage := range stages {
		for operationIndex, operation := range stage.Operations {
			if operation.Name == "run" {
				networks = append(networks, runNetwork{stage.ID, operationIndex, operation.Properties["network"]})
			}
		}
	}
	data, err := json.Marshal(struct {
		Networks []runNetwork `json:"networks,omitempty"`
		AddHosts []string     `json:"add_hosts,omitempty"`
	}{networks, addHosts})
	if err != nil {
		return "", err
	}
	return digest.FromBytes(data), nil
}

func buildAddHostsDigest(addHosts []string) (digest.Digest, error) {
	data, err := json.Marshal(addHosts)
	if err != nil {
		return "", err
	}
	return digest.FromBytes(data), nil
}

func buildResultCacheEligible(addHosts []string) bool {
	for _, entry := range addHosts {
		if strings.HasSuffix(entry, ":host-gateway") {
			return false
		}
	}
	return true
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return make(map[string]string)
	}
	return maps.Clone(source)
}
