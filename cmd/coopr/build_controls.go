package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/strio-dev/coopr/internal/buildah"
	"github.com/strio-dev/coopr/internal/oci"
	"github.com/strio-dev/coopr/internal/transfer"

	"github.com/spf13/cobra"
	buildahcli "go.podman.io/buildah/pkg/cli"
	buildahparse "go.podman.io/buildah/pkg/parse"
	"go.podman.io/common/pkg/auth"
)

type buildControlFlags struct {
	buildahcli.FromAndBudResults
	buildahcli.UserNSResults
	buildahcli.NameSpaceResults
	buildahcli.LayerResults
	buildahcli.BudResults
	mounts   []string
	runtime  string
	command  *cobra.Command
	setupErr error
}

func (flags *buildControlFlags) addTo(command *cobra.Command) {
	flags.command = command
	common, err := buildahcli.GetFromAndBudFlags(&flags.FromAndBudResults, &flags.UserNSResults, &flags.NameSpaceResults)
	flags.setupErr = err
	if err == nil {
		addNativeFlags(command, &common, buildahcli.GetFromAndBudFlagsCompletions(),
			"http-proxy", "dns", "dns-search", "dns-option", "memory", "memory-swap", "cpu-period", "cpu-quota", "cpu-shares", "shm-size", "ulimit", "cdi-config-dir", "volume", "cap-add", "cap-drop", "security-opt", "device", "group-add", "userns", "userns-uid-map", "userns-gid-map", "userns-uid-map-user", "userns-gid-map-group", "cgroupns", "cgroup-parent", "cpuset-cpus", "cpuset-mems", "pid", "ipc", "uts", "isolation")
	}
	bud := buildahcli.GetBudFlags(&flags.BudResults)
	bud.Lookup("jobs").Shorthand = "j"
	addNativeFlags(command, &bud, buildahcli.GetBudFlagsCompletions(), "hooks-dir", "runtime-flag", "rm", "skip-unused-stages", "compat-volumes", "no-hosts", "no-hostname", "jobs")
	command.Flags().Lookup("compat-volumes").Usage = "discard RUN changes under declared image volumes"
	layers := buildahcli.GetLayerFlags(&flags.LayerResults)
	addNativeFlags(command, &layers, nil, "layers", "force-rm", "save-stages", "stage-labels")
	set := command.Flags()
	set.StringArrayVar(&flags.mounts, "mount", nil, "mount into every RUN using Containerfile mount options (repeatable)")
	set.StringVar(&flags.runtime, "runtime", "", "OCI runtime executable for RUN instructions")
	// Coopr caches ordered instructions by default and removes failed containers.
	layerEnv := os.Getenv("BUILDAH_LAYERS")
	setNativeDefault(set.Lookup("layers"), fmt.Sprint(!strings.EqualFold(layerEnv, "false") && layerEnv != "0"))
	setNativeDefault(set.Lookup("force-rm"), "true")
	// Execution resolves these defaults after loading the requested configuration.
	for _, name := range []string{"isolation", "shm-size"} {
		setNativeDefault(set.Lookup(name), "")
	}
	// Keep absence distinct from an explicitly supplied empty list. Runtime
	// configuration (including --module) is loaded when controls are resolved.
	flags.DNSServers, flags.DNSSearch, flags.DNSOptions = nil, nil, nil
	flags.Ulimit, flags.Volumes, flags.Devices, flags.OCIHooksDir = nil, nil, nil, nil
	for _, name := range []string{"dns", "dns-search", "dns-option", "ulimit", "volume", "device", "hooks-dir"} {
		if flag := set.Lookup(name); flag != nil {
			flag.DefValue = "[]"
		}
	}
}

func (flags buildControlFlags) controls() (buildah.RunControls, error) {
	if flags.setupErr != nil {
		return buildah.RunControls{}, flags.setupErr
	}
	var cgroupManager, networkConfigDir string
	var configModules, cdiSpecDirs []string
	if flags.command != nil {
		set := flags.command.Flags()
		cgroupManager, _ = set.GetString("cgroup-manager")
		configModules, _ = set.GetStringArray("module")
		cdiSpecDirs, _ = set.GetStringArray("cdi-spec-dir")
		networkConfigDir, _ = set.GetString("network-config-dir")
	}
	if flags.CDIConfigDir != "" {
		cdiSpecDirs = []string{flags.CDIConfigDir}
	}
	return buildah.ParseRunControls(buildah.RunControlInput{
		HTTPProxy: flags.HTTPProxy, DNSServers: flags.DNSServers, DNSSearch: flags.DNSSearch, DNSOptions: flags.DNSOptions,
		Memory: flags.Memory, MemorySwap: flags.MemorySwap, CPUPeriod: flags.CPUPeriod, CPUQuota: flags.CPUQuota,
		CPUShares: flags.CPUShares, ShmSize: flags.ShmSize, Ulimits: flags.Ulimit,
		Volumes: flags.Volumes, CapAdd: flags.CapAdd, CapDrop: flags.CapDrop, SecurityOptions: flags.SecurityOpt,
		Devices: flags.Devices, GroupAdd: flags.GroupAdd, UserNS: flags.UserNS,
		UIDMap: flags.UserNSUIDMap, GIDMap: flags.UserNSGIDMap, UIDMapUser: flags.UserNSUIDMapUser, GIDMapGroup: flags.UserNSGIDMapGroup,
		CgroupNS: flags.Cgroup, CgroupParent: flags.CgroupParent, CPUSetCPUs: flags.CPUSetCPUs,
		CPUSetMems: flags.CPUSetMems, PIDNS: flags.PID, IPCNS: flags.IPC, UTSNS: flags.UTS,
		HooksDirs:    flags.OCIHooksDir,
		RuntimeFlags: flags.RuntimeFlags,
		Isolation:    flags.Isolation, Runtime: flags.runtime,
		NoHosts: flags.NoHosts, NoHostname: flags.NoHostname, CgroupManager: cgroupManager,
		ConfigModules: configModules, CDISpecDirs: cdiSpecDirs, NetworkConfigDir: networkConfigDir,
	})
}

type registryFlags struct {
	authFile       string
	certDir        string
	tlsVerify      bool
	credentials    string
	retry          uint
	nativeRetry    buildahcli.FromAndBudResults
	retryDelay     time.Duration
	decryptionKeys []string
}

func (flags *registryFlags) addTo(command *cobra.Command) {
	set := command.Flags()
	set.StringVar(&flags.authFile, "authfile", auth.GetDefaultAuthFile(), "path to a containers-auth.json or Docker config.json registry credential file")
	set.StringVar(&flags.certDir, "cert-dir", "", "directory containing ca.crt and optional client certificates")
	set.StringVar(&flags.credentials, "creds", "", "registry username[:password] (overrides authfile)")
	native, setupErr := buildahcli.GetFromAndBudFlags(&flags.nativeRetry, new(buildahcli.UserNSResults), new(buildahcli.NameSpaceResults))
	if setupErr == nil {
		addNativeFlags(command, &native, buildahcli.GetFromAndBudFlagsCompletions(), "retry", "retry-delay")
	}
	before := command.PreRunE
	command.PreRunE = func(cmd *cobra.Command, args []string) error {
		if setupErr != nil {
			return setupErr
		}
		// Match native flag parsing: policy and duration validation happens only
		// when executing, so --help remains available for invalid values.
		if flags.nativeRetry.Retry < 0 {
			return fmt.Errorf("--retry must be non-negative")
		}
		flags.retry = uint(flags.nativeRetry.Retry)
		flags.retryDelay = 0
		if cmd.Flags().Changed("retry-delay") && flags.nativeRetry.RetryDelay != "" {
			delay, err := time.ParseDuration(flags.nativeRetry.RetryDelay)
			if err != nil {
				return fmt.Errorf("unable to parse value provided %q as --retry-delay: %w", flags.nativeRetry.RetryDelay, err)
			}
			flags.retryDelay = delay
		}
		if before != nil {
			return before(cmd, args)
		}
		return nil
	}
	if run := command.RunE; run != nil {
		command.RunE = func(cmd *cobra.Command, args []string) (retErr error) {
			path, owned, err := oci.MaterializeRegistryAuthFile(flags.authFileOption(cmd))
			if err != nil {
				return err
			}
			if owned {
				original := flags.authFile
				flags.authFile = path
				defer func() {
					flags.authFile = original
					retErr = buildah.RemoveWorkerDirectory(path, retErr)
				}()
			}
			return run(cmd, args)
		}
	}
	set.BoolVar(&flags.tlsVerify, "tls-verify", true, "require valid HTTPS certificates for registry connections")
}

func (flags registryFlags) tlsPolicy(command *cobra.Command) *bool {
	if !command.Flags().Changed("tls-verify") {
		return nil
	}
	return &flags.tlsVerify
}

func (flags registryFlags) authFileOption(command *cobra.Command) string {
	if !command.Flags().Changed("authfile") && os.Getenv("REGISTRY_AUTH_FILE") == "" {
		// Leave implicit Docker credentials to native resolution. Passing the default
		// as an explicit path would make an absent config.json mandatory.
		return ""
	}
	return flags.authFile
}

func (flags registryFlags) transferOptions(command *cobra.Command) transfer.Options {
	return transfer.Options{
		AuthFile: flags.authFileOption(command), CertDir: flags.certDir, TLSVerify: flags.tlsPolicy(command),
		Credentials: flags.credentials, Retry: flags.retry, RetrySet: command.Flags().Changed("retry"),
		RetryDelay: flags.retryDelay, DecryptionKeys: flags.decryptionKeys,
		SignaturePolicyPath: commandSignaturePolicy(command),
	}
}

func (flags registryFlags) resolverOptions(command *cobra.Command) oci.Options {
	return flags.transferOptions(command).RegistryOptions()
}

func parseCacheSpecs(values []string, localPathKey string) ([]buildah.CacheSpec, error) {
	result := make([]buildah.CacheSpec, 0, len(values))
	for _, value := range values {
		if !strings.Contains(value, "=") {
			repositories, err := buildahparse.RepoNamesToNamedReferences([]string{value})
			if err != nil {
				return nil, err
			}
			result = append(result, buildah.CacheSpec{Transport: "registry", Reference: repositories[0].Name()})
			continue
		}
		reader := csv.NewReader(strings.NewReader(value))
		fields, err := reader.Read()
		if err != nil {
			return nil, fmt.Errorf("invalid cache %q: %w", value, err)
		}
		if _, err := reader.Read(); err != io.EOF {
			return nil, fmt.Errorf("invalid cache %q: expected one CSV record", value)
		}
		var cacheType, path string
		for _, field := range fields {
			key, fieldValue, ok := strings.Cut(field, "=")
			if !ok {
				return nil, fmt.Errorf("invalid cache option %q: expected KEY=VALUE", field)
			}
			switch strings.ToLower(key) {
			case "type":
				cacheType = fieldValue
			case localPathKey:
				path = fieldValue
			default:
				return nil, fmt.Errorf("unsupported cache option %q: expected type=local,%s=PATH", key, localPathKey)
			}
		}
		if cacheType != "local" || path == "" {
			return nil, fmt.Errorf("invalid cache %q: expected type=local,%s=PATH", value, localPathKey)
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("cache layout path: %w", err)
		}
		result = append(result, buildah.CacheSpec{Transport: "oci-layout", Reference: absolute})
	}
	return result, nil
}

func (flags buildControlFlags) lifecycle() buildah.LifecycleControls {
	return buildah.LifecycleControls{NoLayers: !flags.Layers, KeepIntermediate: !flags.Rm && !flags.ForceRm, KeepFailed: !flags.ForceRm, BuildUnusedStages: !flags.SkipUnusedStages, CompatVolumes: flags.CompatVolumes, SaveStages: flags.SaveStages, StageLabels: flags.StageLabels}
}

func commandSignaturePolicy(cmd *cobra.Command) string {
	value, _ := cmd.Flags().GetString("signature-policy")
	return value
}
