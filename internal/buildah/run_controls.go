package buildah

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/opencontainers/runtime-spec/specs-go"
	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	buildahparse "go.podman.io/buildah/pkg/parse"
	buildahutil "go.podman.io/buildah/pkg/util"
	"go.podman.io/common/pkg/capabilities"
	commonconfig "go.podman.io/common/pkg/config"
	storageTypes "go.podman.io/storage/types"
	"tags.cncf.io/container-device-interface/pkg/parser"
)

// RunControlInput is the user-facing representation of build-wide RUN
// controls. ParseRunControls validates and canonicalizes it before the values
// cross a worker boundary or enter a cache identity.
type RunControlInput struct {
	HTTPProxy        bool
	DNSServers       []string
	DNSSearch        []string
	DNSOptions       []string
	Memory           string
	MemorySwap       string
	CPUPeriod        uint64
	CPUQuota         int64
	CPUShares        uint64
	ShmSize          string
	Ulimits          []string
	Volumes          []string
	CapAdd           []string
	CapDrop          []string
	SecurityOptions  []string
	Devices          []string
	GroupAdd         []string
	UserNS           string
	UIDMap           []string
	GIDMap           []string
	UIDMapUser       string
	GIDMapGroup      string
	CgroupNS         string
	CgroupParent     string
	CPUSetCPUs       string
	CPUSetMems       string
	PIDNS            string
	IPCNS            string
	UTSNS            string
	HooksDirs        []string
	RuntimeFlags     []string
	Isolation        string
	Runtime          string
	NoHostname       bool
	NoHosts          bool
	CgroupManager    string
	ConfigModules    []string
	CDISpecDirs      []string
	NetworkConfigDir string
	NetworkCmdPath   string
}

// RunControls contains canonical execution settings for every RUN in a build.
// Numeric sizes are bytes.  Zero is generally the upstream unset value;
// ShmSizeSet preserves an explicitly requested zero because Buildah forwards
// it as a tmpfs size option.
type RunControls struct {
	HTTPProxy        bool                     `json:"http_proxy,omitempty"`
	DNSServers       []string                 `json:"dns_servers,omitempty"`
	DNSSearch        []string                 `json:"dns_search,omitempty"`
	DNSOptions       []string                 `json:"dns_options,omitempty"`
	Memory           int64                    `json:"memory,omitempty"`
	MemorySwap       int64                    `json:"memory_swap,omitempty"`
	CPUPeriod        uint64                   `json:"cpu_period,omitempty"`
	CPUQuota         int64                    `json:"cpu_quota,omitempty"`
	CPUShares        uint64                   `json:"cpu_shares,omitempty"`
	ShmSize          int64                    `json:"shm_size,omitempty"`
	ShmSizeSet       bool                     `json:"shm_size_set,omitempty"`
	Ulimits          []string                 `json:"ulimits,omitempty"`
	Volumes          []string                 `json:"volumes,omitempty"`
	CapAdd           []string                 `json:"cap_add,omitempty"`
	CapDrop          []string                 `json:"cap_drop,omitempty"`
	NoNewPrivileges  bool                     `json:"no_new_privileges,omitempty"`
	LabelOptions     []string                 `json:"label_options,omitempty"`
	Masks            []string                 `json:"masks,omitempty"`
	Unmasks          []string                 `json:"unmasks,omitempty"`
	SeccompProfile   string                   `json:"seccomp_profile,omitempty"`
	ApparmorProfile  string                   `json:"apparmor_profile,omitempty"`
	Devices          []string                 `json:"devices,omitempty"`
	GroupAdd         []string                 `json:"group_add,omitempty"`
	NamespaceOptions define.NamespaceOptions  `json:"namespace_options,omitempty"`
	IDMappingOptions *define.IDMappingOptions `json:"id_mapping_options,omitempty"`
	CgroupParent     string                   `json:"cgroup_parent,omitempty"`
	CPUSetCPUs       string                   `json:"cpuset_cpus,omitempty"`
	CPUSetMems       string                   `json:"cpuset_mems,omitempty"`
	HooksDirs        []string                 `json:"hooks_dirs,omitempty"`
	HooksDigests     []string                 `json:"hooks_digests,omitempty"`
	SeccompDigest    string                   `json:"seccomp_digest,omitempty"`
	RuntimeFlags     []string                 `json:"runtime_flags,omitempty"`
	Isolation        string                   `json:"isolation,omitempty"`
	Runtime          string                   `json:"runtime,omitempty"`
	NoHostname       bool                     `json:"no_hostname,omitempty"`
	NoHosts          bool                     `json:"no_hosts,omitempty"`
	CgroupManager    string                   `json:"cgroup_manager,omitempty"`
	CgroupManagerSet bool                     `json:"cgroup_manager_set,omitempty"`
	ConfigModules    []string                 `json:"config_modules,omitempty"`
	CDISpecDirs      []string                 `json:"cdi_spec_dirs,omitempty"`
	NetworkConfigDir string                   `json:"network_config_dir,omitempty"`
	NetworkCmdPath   string                   `json:"network_cmd_path,omitempty"`
	BaseCapabilities []string                 `json:"base_capabilities,omitempty"`
}

func ParseRunControls(input RunControlInput) (RunControls, error) {
	containerConfig, err := loadRunContainerConfig(input.ConfigModules)
	if err != nil {
		return RunControls{}, err
	}
	controls := RunControls{
		HTTPProxy: input.HTTPProxy, CPUPeriod: input.CPUPeriod,
		CPUQuota: input.CPUQuota, CPUShares: input.CPUShares,
		NoHostname: input.NoHostname, NoHosts: input.NoHosts,
		ConfigModules:    slices.Clone(containerConfig.LoadedModules()),
		BaseCapabilities: slices.Clone(containerConfig.Containers.DefaultCapabilities.Get()),
	}
	controls.CgroupManager = strings.TrimSpace(input.CgroupManager)
	controls.CgroupManagerSet = controls.CgroupManager != ""
	if controls.CgroupManager == "" {
		controls.CgroupManager = containerConfig.Engine.CgroupManager
	}
	if err := validateCgroupManager(controls.CgroupManager); err != nil {
		return RunControls{}, err
	}
	if input.CDISpecDirs == nil {
		controls.CDISpecDirs = slices.Clone(containerConfig.Engine.CdiSpecDirs.Get())
	} else {
		controls.CDISpecDirs, err = normalizeAbsolutePaths("cdi-spec-dir", input.CDISpecDirs)
		if err != nil {
			return RunControls{}, err
		}
	}
	if input.NetworkConfigDir != "" {
		directories, err := normalizeAbsolutePaths("network-config-dir", []string{input.NetworkConfigDir})
		if err != nil {
			return RunControls{}, err
		}
		controls.NetworkConfigDir = directories[0]
	}
	if input.NetworkCmdPath != "" {
		controls.NetworkCmdPath, err = filepath.Abs(input.NetworkCmdPath)
		if err != nil {
			return RunControls{}, fmt.Errorf("network-cmd-path: %w", err)
		}
		controls.NetworkCmdPath = filepath.Clean(controls.NetworkCmdPath)
	}
	if input.Memory != "" {
		controls.Memory, err = parseNonNegativeBytes("memory limit", input.Memory)
		if err != nil {
			return RunControls{}, err
		}
	}
	if input.MemorySwap != "" {
		if input.MemorySwap == "-1" {
			controls.MemorySwap = -1
		} else {
			controls.MemorySwap, err = parseNonNegativeBytes("memory-swap limit", input.MemorySwap)
			if err != nil {
				return RunControls{}, err
			}
		}
	}
	shmSize := input.ShmSize
	if shmSize == "" {
		shmSize = containerConfig.Containers.ShmSize
	}
	if shmSize != "" {
		controls.ShmSize, err = parseNonNegativeBytes("shm-size", shmSize)
		if err != nil {
			return RunControls{}, err
		}
		controls.ShmSizeSet = true
	}
	dnsServers := input.DNSServers
	if dnsServers == nil {
		dnsServers = containerConfig.Containers.DNSServers.Get()
	}
	controls.DNSServers, err = normalizeDNSServers(dnsServers)
	if err != nil {
		return RunControls{}, err
	}
	dnsSearch := input.DNSSearch
	if dnsSearch == nil {
		dnsSearch = containerConfig.Containers.DNSSearches.Get()
	}
	controls.DNSSearch, err = normalizeDNSSearch(dnsSearch)
	if err != nil {
		return RunControls{}, err
	}
	dnsOptions := input.DNSOptions
	if dnsOptions == nil {
		dnsOptions = containerConfig.Containers.DNSOptions.Get()
	}
	controls.DNSOptions, err = normalizeDNSOptions(dnsOptions)
	if err != nil {
		return RunControls{}, err
	}
	ulimits := input.Ulimits
	if ulimits == nil {
		ulimits = containerConfig.Containers.DefaultUlimits.Get()
	}
	for _, value := range ulimits {
		limit, parseErr := buildahutil.ParseUlimit(value)
		if parseErr != nil {
			return RunControls{}, fmt.Errorf("invalid ulimit %q: %w", value, parseErr)
		}
		controls.Ulimits = append(controls.Ulimits, fmt.Sprintf("%s=%d:%d", limit.Name, limit.Soft, limit.Hard))
	}
	volumes := input.Volumes
	if volumes == nil {
		volumes = containerConfig.Volumes()
	}
	if err := buildahparse.Volumes(volumes); err != nil {
		return RunControls{}, fmt.Errorf("invalid volume: %w", err)
	}
	controls.Volumes = deduplicateTrimmed(volumes)
	controls.CapAdd, err = capabilities.NormalizeCapabilities(input.CapAdd)
	if err != nil {
		return RunControls{}, fmt.Errorf("cap-add: %w", err)
	}
	controls.CapDrop, err = capabilities.NormalizeCapabilities(input.CapDrop)
	if err != nil {
		return RunControls{}, fmt.Errorf("cap-drop: %w", err)
	}
	if _, err := capabilities.MergeCapabilities(nil, controls.CapAdd, controls.CapDrop); err != nil {
		return RunControls{}, err
	}
	securityOptions := append(containerConfig.SecurityOptions(), input.SecurityOptions...)
	if !hasSecurityOption(input.SecurityOptions, "seccomp") && !hasSecurityOption(securityOptions, "seccomp") {
		if profile := defaultSeccompProfile(); profile != "" {
			securityOptions = append(securityOptions, "seccomp="+profile)
		}
	}
	if err := controls.parseSecurityOptions(securityOptions); err != nil {
		return RunControls{}, err
	}
	devices := input.Devices
	if devices == nil {
		devices = containerConfig.Devices()
	}
	for _, device := range devices {
		device = strings.TrimSpace(device)
		if device == "" {
			return RunControls{}, fmt.Errorf("device must not be empty")
		}
		source, _, _, parseErr := buildahparse.Device(device)
		if parseErr != nil && !parser.IsQualifiedName(device) {
			return RunControls{}, fmt.Errorf("invalid device %q: %w", device, parseErr)
		}
		if parseErr == nil && !filepath.IsAbs(source) {
			return RunControls{}, fmt.Errorf("device source %q must be absolute", source)
		}
		controls.Devices = appendUniqueRunControl(controls.Devices, device)
	}
	controls.GroupAdd, err = normalizeUniqueValues("group-add", input.GroupAdd)
	if err != nil {
		return RunControls{}, err
	}
	controls.CgroupParent = strings.TrimSpace(input.CgroupParent)
	controls.CPUSetCPUs = strings.TrimSpace(input.CPUSetCPUs)
	controls.CPUSetMems = strings.TrimSpace(input.CPUSetMems)
	hooksDirs := input.HooksDirs
	if hooksDirs == nil {
		hooksDirs = existingDirectories(containerConfig.Engine.HooksDir.Get())
	}
	controls.HooksDirs, err = normalizeAbsoluteDirectories("hooks-dir", hooksDirs)
	if err != nil {
		return RunControls{}, err
	}
	for _, directory := range controls.HooksDirs {
		digest, digestErr := digestDirectoryFiles(directory)
		if digestErr != nil {
			return RunControls{}, fmt.Errorf("digest hooks-dir %q: %w", directory, digestErr)
		}
		controls.HooksDigests = append(controls.HooksDigests, digest)
	}
	controls.RuntimeFlags, err = normalizeRequiredValues("runtime-flag", input.RuntimeFlags)
	if err != nil {
		return RunControls{}, err
	}
	for index, flag := range controls.RuntimeFlags {
		if strings.HasPrefix(flag, "-") {
			return RunControls{}, fmt.Errorf("runtime-flag %q must omit leading dashes", flag)
		}
		controls.RuntimeFlags[index] = "--" + flag
	}
	controls.Isolation = strings.ToLower(strings.TrimSpace(input.Isolation))
	if controls.Isolation != "" {
		if _, err := buildahparse.IsolationOption(controls.Isolation); err != nil {
			return RunControls{}, fmt.Errorf("isolation: %w", err)
		}
	}
	controls.Runtime = strings.TrimSpace(input.Runtime)
	controls.NamespaceOptions, controls.IDMappingOptions, err = parseRunNamespaces(input)
	if err != nil {
		return RunControls{}, err
	}
	return controls, nil
}

func loadRunContainerConfig(modules []string) (*commonconfig.Config, error) {
	config, err := commonconfig.New(&commonconfig.Options{Modules: slices.Clone(modules)})
	if err != nil {
		return nil, fmt.Errorf("load containers configuration: %w", err)
	}
	return config, nil
}

// ConfigureRuntimeConfig installs the same containers.conf module selection
// used while parsing RunControls as Buildah's process default. Build workers
// are isolated child processes, so Buildah's package-global configuration is
// scoped to one supervised build rather than shared across concurrent builds.
func ConfigureRuntimeConfig(controls RunControls) error {
	config, err := commonconfig.New(&commonconfig.Options{Modules: slices.Clone(controls.ConfigModules), SetDefault: true})
	if err != nil {
		return fmt.Errorf("load build worker containers configuration: %w", err)
	}
	if controls.CgroupManager != "" {
		config.Engine.CgroupManager = controls.CgroupManager
	}
	if controls.CDISpecDirs != nil {
		config.Engine.CdiSpecDirs.Set(slices.Clone(controls.CDISpecDirs))
	}
	return nil
}

func validateCgroupManager(value string) error {
	switch value {
	case "", commonconfig.SystemdCgroupsManager, commonconfig.CgroupfsCgroupsManager:
		return nil
	default:
		return fmt.Errorf("invalid cgroup manager %q: expected systemd or cgroupfs", value)
	}
}

func hasSecurityOption(options []string, name string) bool {
	prefix := name + "="
	return slices.ContainsFunc(options, func(option string) bool {
		return strings.HasPrefix(strings.TrimSpace(option), prefix)
	})
}

func defaultSeccompProfile() string {
	for _, path := range []string{buildahparse.SeccompOverridePath, buildahparse.SeccompDefaultPath} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	return ""
}

func deduplicateTrimmed(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = appendUniqueRunControl(result, value)
		}
	}
	return result
}

func normalizeUniqueValues(name string, values []string) ([]string, error) {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%s value must not be empty", name)
		}
	}
	return deduplicateTrimmed(values), nil
}

func normalizeRequiredValues(name string, values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("%s value must not be empty", name)
		}
		result = append(result, value)
	}
	return result, nil
}

func normalizeAbsoluteDirectories(name string, values []string) ([]string, error) {
	result, err := normalizeAbsolutePaths(name, values)
	if err != nil {
		return nil, err
	}
	for _, absolute := range result {
		info, err := os.Stat(absolute)
		if err != nil {
			return nil, fmt.Errorf("inspect %s %q: %w", name, absolute, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s %q is not a directory", name, absolute)
		}
	}
	return result, nil
}

func normalizeAbsolutePaths(name string, values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("%s value must not be empty", name)
		}
		absolute, err := filepath.Abs(value)
		if err != nil {
			return nil, fmt.Errorf("resolve %s %q: %w", name, value, err)
		}
		result = appendUniqueRunControl(result, absolute)
	}
	return result, nil
}

func existingDirectories(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if info, err := os.Stat(value); err == nil && info.IsDir() {
			result = append(result, value)
		}
	}
	return result
}

func (controls *RunControls) parseSecurityOptions(values []string) error {
	for _, option := range values {
		option = strings.TrimSpace(option)
		if option == "no-new-privileges" {
			controls.NoNewPrivileges = true
			continue
		}
		name, value, found := strings.Cut(option, "=")
		if !found || value == "" {
			return fmt.Errorf("invalid security-opt %q: expected name=value", option)
		}
		switch name {
		case "label":
			controls.LabelOptions = appendUniqueRunControl(controls.LabelOptions, value)
		case "apparmor":
			controls.ApparmorProfile = value
		case "seccomp":
			if value != "unconfined" {
				absolute, err := filepath.Abs(value)
				if err != nil {
					return fmt.Errorf("resolve seccomp profile %q: %w", value, err)
				}
				if info, err := os.Stat(absolute); err != nil {
					return fmt.Errorf("inspect seccomp profile %q: %w", value, err)
				} else if !info.Mode().IsRegular() {
					return fmt.Errorf("seccomp profile %q is not a regular file", value)
				}
				value = absolute
				digest, err := digestFile(absolute)
				if err != nil {
					return fmt.Errorf("digest seccomp profile %q: %w", value, err)
				}
				controls.SeccompDigest = digest
			}
			controls.SeccompProfile = value
		case "mask":
			for _, path := range strings.Split(value, ":") {
				controls.Masks = appendUniqueRunControl(controls.Masks, path)
			}
		case "unmask":
			for _, path := range strings.Split(value, ":") {
				matches, _ := filepath.Glob(path)
				if len(matches) == 0 {
					matches = []string{path}
				}
				for _, match := range matches {
					controls.Unmasks = appendUniqueRunControl(controls.Unmasks, match)
				}
			}
		default:
			return fmt.Errorf("invalid security-opt %q", option)
		}
	}
	return nil
}

func digestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func digestDirectoryFiles(path string) (string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(path, entry.Name()))
		if err != nil {
			return "", err
		}
		_, _ = io.WriteString(hash, entry.Name())
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(data)
		_, _ = hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func parseRunNamespaces(input RunControlInput) (define.NamespaceOptions, *define.IDMappingOptions, error) {
	var namespaces define.NamespaceOptions
	for _, namespace := range []struct{ name, value string }{
		{string(specs.CgroupNamespace), input.CgroupNS},
		{string(specs.PIDNamespace), input.PIDNS},
		{string(specs.IPCNamespace), input.IPCNS},
		{string(specs.UTSNamespace), input.UTSNS},
	} {
		option, set, err := parseRunNamespace(namespace.name, namespace.value)
		if err != nil {
			return nil, nil, err
		}
		if set {
			namespaces.AddOrReplace(option)
		}
	}
	auto := strings.HasPrefix(input.UserNS, "auto")
	if auto {
		if len(input.UIDMap) != 0 || len(input.GIDMap) != 0 || input.UIDMapUser != "" || input.GIDMapGroup != "" {
			return nil, nil, fmt.Errorf("userns auto mappings must be specified in the auto options")
		}
		autoOptions, err := buildahparse.GetAutoOptions(input.UserNS)
		if err != nil {
			return nil, nil, fmt.Errorf("parse userns %q: %w", input.UserNS, err)
		}
		userOption := define.NamespaceOption{Name: string(specs.UserNamespace)}
		mapping := &define.IDMappingOptions{AutoUserNs: true, AutoUserNsOpts: *autoOptions}
		namespaces.AddOrReplace(userOption)
		return namespaces, mapping, nil
	}
	userOption, userSet, err := parseRunNamespace(string(specs.UserNamespace), input.UserNS)
	if err != nil {
		return nil, nil, err
	}
	hasMappings := len(input.UIDMap) != 0 || len(input.GIDMap) != 0 || input.UIDMapUser != "" || input.GIDMapGroup != ""
	if !userSet && !hasMappings {
		return namespaces, nil, nil
	}
	if !userSet {
		userOption = define.NamespaceOption{Name: string(specs.UserNamespace)}
	}
	if userOption.Host && hasMappings {
		return nil, nil, fmt.Errorf("cannot specify ID mappings with host user namespace")
	}
	mapUser, mapGroup := input.UIDMapUser, input.GIDMapGroup
	if mapUser == "" {
		mapUser = mapGroup
	}
	if mapGroup == "" {
		mapGroup = mapUser
	}
	mapping := &define.IDMappingOptions{HostUIDMapping: userOption.Host, HostGIDMapping: userOption.Host}
	if hasMappings {
		parsed, err := storageTypes.ParseIDMapping(input.UIDMap, input.GIDMap, mapUser, mapGroup)
		if err != nil {
			return nil, nil, fmt.Errorf("parse user namespace mappings: %w", err)
		}
		mapping.HostUIDMapping = parsed.HostUIDMapping
		mapping.HostGIDMapping = parsed.HostGIDMapping
		for _, value := range parsed.UIDMap {
			mapping.UIDMap = append(mapping.UIDMap, specs.LinuxIDMapping{ContainerID: uint32(value.ContainerID), HostID: uint32(value.HostID), Size: uint32(value.Size)})
		}
		for _, value := range parsed.GIDMap {
			mapping.GIDMap = append(mapping.GIDMap, specs.LinuxIDMapping{ContainerID: uint32(value.ContainerID), HostID: uint32(value.HostID), Size: uint32(value.Size)})
		}
	}
	namespaces.AddOrReplace(userOption)
	return namespaces, mapping, nil
}

func parseRunNamespace(name, value string) (define.NamespaceOption, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return define.NamespaceOption{}, false, nil
	}
	option := define.NamespaceOption{Name: name}
	switch value {
	case "private", "container":
		return option, true, nil
	case "host":
		option.Host = true
		return option, true, nil
	}
	value = strings.TrimPrefix(value, "ns:")
	if value == "" {
		return define.NamespaceOption{}, false, fmt.Errorf("%s namespace path must not be empty", name)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return define.NamespaceOption{}, false, fmt.Errorf("resolve %s namespace %q: %w", name, value, err)
	}
	if _, err := os.Stat(absolute); err != nil {
		return define.NamespaceOption{}, false, fmt.Errorf("inspect %s namespace %q: %w", name, value, err)
	}
	option.Path = absolute
	return option, true, nil
}

// parseNonNegativeBytes accepts Buildah's binary RAM units (b, k, m, g, t, p,
// optionally suffixed with b or ib), without reading host container defaults.
func parseNonNegativeBytes(name, value string) (int64, error) {
	invalid := fmt.Errorf("invalid %s %q: expected a nonnegative byte size", name, value)
	split := strings.LastIndexAny(value, "0123456789. ")
	if split < 0 {
		return 0, invalid
	}
	number := value[:split+1]
	if value[split] == ' ' {
		number = value[:split]
	}
	size, err := strconv.ParseFloat(number, 64)
	if err != nil || size < 0 {
		return 0, invalid
	}
	suffix := strings.ToLower(value[split+1:])
	if suffix != "" && suffix != "b" {
		power := strings.IndexByte("kmgtp", suffix[0]) + 1
		if power == 0 || (len(suffix) != 1 && suffix[1:] != "b" && suffix[1:] != "ib") {
			return 0, invalid
		}
		size = math.Ldexp(size, power*10)
	}
	if size < 0 || size >= math.Exp2(63) || math.IsNaN(size) {
		return 0, invalid
	}
	return int64(size), nil
}

func normalizeDNSServers(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if strings.EqualFold(value, "none") {
			if len(values) != 1 {
				return nil, fmt.Errorf("dns value none cannot be combined with a server")
			}
			return []string{"none"}, nil
		}
		ip := net.ParseIP(value)
		if ip == nil {
			return nil, fmt.Errorf("invalid DNS server %q", value)
		}
		result = appendUniqueRunControl(result, ip.String())
	}
	return result, nil
}

func normalizeDNSSearch(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "." {
			result = appendUniqueRunControl(result, value)
			continue
		}
		if value == "" || len(value) > 253 || strings.ContainsAny(value, " \t\r\n\x00") {
			return nil, fmt.Errorf("invalid DNS search domain %q", value)
		}
		for _, label := range strings.Split(strings.TrimSuffix(value, "."), ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return nil, fmt.Errorf("invalid DNS search domain %q", value)
			}
			for _, char := range label {
				if char != '-' && (char < 'a' || char > 'z') && (char < '0' || char > '9') {
					return nil, fmt.Errorf("invalid DNS search domain %q", value)
				}
			}
		}
		result = appendUniqueRunControl(result, value)
	}
	return result, nil
}

func normalizeDNSOptions(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || strings.ContainsAny(value, " \t\r\n\x00,") {
			return nil, fmt.Errorf("invalid DNS option %q", value)
		}
		result = appendUniqueRunControl(result, value)
	}
	return result, nil
}

func appendUniqueRunControl(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func (controls RunControls) commonBuildOptions(addHosts []string) *define.CommonBuildOptions {
	options := &define.CommonBuildOptions{
		AddHost: slices.Clone(addHosts), HTTPProxy: controls.HTTPProxy,
		DNSServers: slices.Clone(controls.DNSServers), DNSSearch: slices.Clone(controls.DNSSearch), DNSOptions: slices.Clone(controls.DNSOptions),
		Memory: controls.Memory, MemorySwap: controls.MemorySwap,
		CPUPeriod: controls.CPUPeriod, CPUQuota: controls.CPUQuota, CPUShares: controls.CPUShares,
		Ulimit:  slices.Clone(controls.Ulimits),
		Volumes: slices.Clone(controls.Volumes), NoNewPrivileges: controls.NoNewPrivileges,
		LabelOpts: slices.Clone(controls.LabelOptions), Masks: slices.Clone(controls.Masks), Unmasks: slices.Clone(controls.Unmasks),
		SeccompProfilePath: controls.SeccompProfile, ApparmorProfile: controls.ApparmorProfile,
		CgroupParent: controls.CgroupParent, CPUSetCPUs: controls.CPUSetCPUs, CPUSetMems: controls.CPUSetMems,
		OCIHooksDir: slices.Clone(controls.HooksDirs),
		NoHostname:  controls.NoHostname, NoHosts: controls.NoHosts,
	}
	if controls.ShmSizeSet || controls.ShmSize != 0 {
		options.ShmSize = strconv.FormatInt(controls.ShmSize, 10)
	}
	return options
}

// applyBuilderOptions applies canonical build-wide RUN controls to the public
// Buildah builder options. Keeping this operation in one place makes direct,
// graph, and component builds use exactly the same runtime configuration.
func (controls RunControls) applyBuilderOptions(options *upstream.BuilderOptions) error {
	if options == nil {
		return fmt.Errorf("apply RUN controls to nil builder options")
	}
	if err := validateRunControls(controls); err != nil {
		return err
	}
	baseCapabilities := options.Capabilities
	if controls.BaseCapabilities != nil {
		baseCapabilities = controls.BaseCapabilities
	}
	mergedCapabilities, err := capabilities.MergeCapabilities(baseCapabilities, controls.CapAdd, controls.CapDrop)
	if err != nil {
		return fmt.Errorf("apply RUN capabilities: %w", err)
	}
	options.Capabilities = mergedCapabilities
	options.GroupAdd = slices.Clone(controls.GroupAdd)
	options.NamespaceOptions = slices.Clone(controls.NamespaceOptions)
	if controls.IDMappingOptions != nil {
		mapping := *controls.IDMappingOptions
		mapping.UIDMap = slices.Clone(controls.IDMappingOptions.UIDMap)
		mapping.GIDMap = slices.Clone(controls.IDMappingOptions.GIDMap)
		options.IDMappingOptions = &mapping
	}
	options.DeviceSpecs = slices.Clone(controls.Devices)
	if controls.Isolation != "" {
		isolation, err := buildahparse.IsolationOption(controls.Isolation)
		if err != nil {
			return fmt.Errorf("isolation: %w", err)
		}
		options.Isolation = isolation
	}
	if controls.CDISpecDirs != nil {
		options.CDIConfigDir = ""
	}
	return nil
}

func validateRunControls(controls RunControls) error {
	if controls.Memory < 0 || controls.MemorySwap < -1 || controls.ShmSize < 0 {
		return fmt.Errorf("invalid negative RUN resource control")
	}
	if err := buildahparse.Volumes(controls.Volumes); err != nil {
		return fmt.Errorf("invalid volume: %w", err)
	}
	if _, err := capabilities.MergeCapabilities(nil, controls.CapAdd, controls.CapDrop); err != nil {
		return err
	}
	if controls.SeccompProfile != "" && controls.SeccompProfile != "unconfined" {
		digest, err := digestFile(controls.SeccompProfile)
		if err != nil {
			return fmt.Errorf("digest seccomp profile %q: %w", controls.SeccompProfile, err)
		}
		if controls.SeccompDigest == "" || digest != controls.SeccompDigest {
			return fmt.Errorf("seccomp profile %q changed after RUN controls were parsed", controls.SeccompProfile)
		}
	}
	if len(controls.HooksDirs) != len(controls.HooksDigests) {
		return fmt.Errorf("OCI hooks directory identities are incomplete")
	}
	for index, directory := range controls.HooksDirs {
		digest, err := digestDirectoryFiles(directory)
		if err != nil {
			return fmt.Errorf("digest hooks-dir %q: %w", directory, err)
		}
		if digest != controls.HooksDigests[index] {
			return fmt.Errorf("hooks-dir %q changed after RUN controls were parsed", directory)
		}
	}
	for _, flag := range controls.RuntimeFlags {
		if !strings.HasPrefix(flag, "--") || len(flag) == 2 {
			return fmt.Errorf("invalid canonical runtime flag %q", flag)
		}
	}
	if controls.Isolation != "" {
		if _, err := buildahparse.IsolationOption(controls.Isolation); err != nil {
			return fmt.Errorf("isolation: %w", err)
		}
	}
	if err := validateCgroupManager(controls.CgroupManager); err != nil {
		return err
	}
	return nil
}

func validateRunControlsForNetwork(controls RunControls, network string) error {
	if network == "none" && (len(controls.DNSServers) != 0 || len(controls.DNSSearch) != 0 || len(controls.DNSOptions) != 0) {
		return fmt.Errorf("DNS controls cannot be used with build network mode none")
	}
	return nil
}

func validateResourceControlEnvironment(controls RunControls, rootless, unified bool, controllers []string, probeErr error) error {
	needsMemory := controls.Memory != 0 || controls.MemorySwap != 0
	needsCPU := controls.CPUPeriod != 0 || controls.CPUQuota != 0 || controls.CPUShares != 0 || controls.CPUSetCPUs != "" || controls.CPUSetMems != ""
	if !needsMemory && !needsCPU {
		return nil
	}
	if probeErr != nil {
		return fmt.Errorf("inspect host cgroup support for RUN resource limits: %w", probeErr)
	}
	if rootless && !unified {
		return fmt.Errorf("RUN resource limits require cgroup v2 for rootless builds")
	}
	if unified {
		if needsMemory && !slices.Contains(controllers, "memory") {
			return fmt.Errorf("RUN memory limits require the memory cgroup controller delegated to Coopr")
		}
		if needsCPU && !slices.Contains(controllers, "cpu") {
			return fmt.Errorf("RUN CPU limits require the cpu cgroup controller delegated to Coopr")
		}
		if (controls.CPUSetCPUs != "" || controls.CPUSetMems != "") && !slices.Contains(controllers, "cpuset") {
			return fmt.Errorf("RUN CPU set limits require the cpuset cgroup controller delegated to Coopr")
		}
	}
	return nil
}
