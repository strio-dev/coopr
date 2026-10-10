package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"coopr/internal/buildah"
	"coopr/internal/oci"
	"coopr/internal/transfer"

	"github.com/spf13/cobra"
	buildahparse "go.podman.io/buildah/pkg/parse"
)

type buildControlFlags struct {
	layers, remove, forceRemove, skipUnused, compatVolumes bool
	saveStages, stageLabels                                bool
	mounts                                                 []string
	cdiConfigDir                                           string
	httpProxy                                              bool
	dns                                                    []string
	dnsSearch                                              []string
	dnsOption                                              []string
	memory                                                 string
	memorySwap                                             string
	cpuPeriod                                              uint64
	cpuQuota                                               int64
	cpuShares                                              uint64
	shmSize                                                string
	ulimits                                                []string
	volumes                                                []string
	capAdd                                                 []string
	capDrop                                                []string
	security                                               []string
	devices                                                []string
	groupAdd                                               []string
	userNS                                                 string
	uidMap                                                 []string
	gidMap                                                 []string
	mapUser                                                string
	mapGroup                                               string
	cgroupNS                                               string
	cgroupParent                                           string
	cpusetCPUs                                             string
	cpusetMems                                             string
	pidNS                                                  string
	ipcNS                                                  string
	utsNS                                                  string
	hooksDirs                                              []string
	runtimeFlags                                           []string
	isolation                                              string
	runtime                                                string
	noHosts, noHostname                                    bool
	command                                                *cobra.Command
	jobs                                                   int
}

func (flags *buildControlFlags) addTo(command *cobra.Command) {
	flags.command = command
	set := command.Flags()
	set.BoolVar(&flags.httpProxy, "http-proxy", true, "pass host HTTP proxy environment variables to RUN instructions")
	set.StringArrayVar(&flags.dns, "dns", nil, "set a DNS server for RUN instructions (repeatable; use none to disable DNS configuration)")
	set.StringArrayVar(&flags.dnsSearch, "dns-search", nil, "set a DNS search domain for RUN instructions (repeatable)")
	set.StringArrayVar(&flags.dnsOption, "dns-option", nil, "set a DNS resolver option for RUN instructions (repeatable)")
	set.StringVarP(&flags.memory, "memory", "m", "", "set the memory limit for RUN instructions")
	set.StringVar(&flags.memorySwap, "memory-swap", "", "set memory plus swap for RUN instructions (-1 for unlimited swap)")
	set.Uint64Var(&flags.cpuPeriod, "cpu-period", 0, "set the CPU CFS period for RUN instructions")
	set.Int64Var(&flags.cpuQuota, "cpu-quota", 0, "set the CPU CFS quota for RUN instructions (-1 for unlimited)")
	set.Uint64VarP(&flags.cpuShares, "cpu-shares", "c", 0, "set relative CPU shares for RUN instructions")
	set.StringVar(&flags.shmSize, "shm-size", "", "set /dev/shm size for RUN instructions")
	set.StringArrayVar(&flags.ulimits, "ulimit", nil, "set a ulimit for RUN instructions as NAME=SOFT:HARD (repeatable)")
	set.StringArrayVar(&flags.mounts, "mount", nil, "mount into every RUN using Containerfile mount options (repeatable)")
	set.StringVar(&flags.cdiConfigDir, "cdi-config-dir", "", "directory of CDI configuration files")
	_ = set.MarkHidden("cdi-config-dir")
	set.StringArrayVarP(&flags.volumes, "volume", "v", nil, "bind mount HOST:CONTAINER[:OPTIONS] into every RUN (repeatable)")
	set.StringSliceVar(&flags.capAdd, "cap-add", nil, "add Linux capabilities to RUN instructions (repeatable)")
	set.StringSliceVar(&flags.capDrop, "cap-drop", nil, "drop Linux capabilities from RUN instructions (repeatable)")
	set.StringArrayVar(&flags.security, "security-opt", nil, "set RUN security options (repeatable)")
	set.StringArrayVar(&flags.devices, "device", nil, "add a host device to RUN instructions (repeatable)")
	set.StringSliceVar(&flags.groupAdd, "group-add", nil, "add supplementary groups to RUN instructions (repeatable)")
	set.StringVar(&flags.userNS, "userns", "", "RUN user namespace: host, private, auto, or namespace path")
	set.StringSliceVar(&flags.uidMap, "userns-uid-map", nil, "map container UID ranges as CONTAINER:HOST:SIZE")
	set.StringSliceVar(&flags.gidMap, "userns-gid-map", nil, "map container GID ranges as CONTAINER:HOST:SIZE")
	set.StringVar(&flags.mapUser, "userns-uid-map-user", "", "use subordinate UID mappings for USER")
	set.StringVar(&flags.mapGroup, "userns-gid-map-group", "", "use subordinate GID mappings for GROUP")
	set.StringVar(&flags.cgroupNS, "cgroupns", "", "RUN cgroup namespace: host, private, or namespace path")
	set.StringVar(&flags.cgroupParent, "cgroup-parent", "", "parent cgroup for RUN instructions")
	set.StringVar(&flags.cpusetCPUs, "cpuset-cpus", "", "CPUs in which RUN instructions may execute")
	set.StringVar(&flags.cpusetMems, "cpuset-mems", "", "memory nodes available to RUN instructions")
	set.StringVar(&flags.pidNS, "pid", "", "RUN PID namespace: host, private, or namespace path")
	set.StringVar(&flags.ipcNS, "ipc", "", "RUN IPC namespace: host, private, or namespace path")
	set.StringVar(&flags.utsNS, "uts", "", "RUN UTS namespace: host, private, or namespace path")
	set.StringArrayVar(&flags.hooksDirs, "hooks-dir", nil, "OCI hooks directory for RUN instructions (repeatable)")
	set.StringSliceVar(&flags.runtimeFlags, "runtime-flag", nil, "pass a global flag to the OCI runtime for RUN instructions (repeatable)")
	set.StringVar(&flags.isolation, "isolation", "", "RUN isolation: oci, rootless, or chroot")
	set.StringVar(&flags.runtime, "runtime", "", "OCI runtime executable for RUN instructions")
	layers := os.Getenv("BUILDAH_LAYERS")
	set.BoolVar(&flags.layers, "layers", !strings.EqualFold(layers, "false") && layers != "0", "cache instructions and preserve their filesystem layers")
	set.BoolVar(&flags.remove, "rm", true, "remove intermediate containers after a successful build")
	set.BoolVar(&flags.forceRemove, "force-rm", true, "also remove intermediate containers after build failure")
	set.BoolVar(&flags.skipUnused, "skip-unused-stages", true, "build only stages required by the selected target")
	set.BoolVar(&flags.saveStages, "save-stages", false, "retain completed intermediate stage images")
	set.BoolVar(&flags.stageLabels, "stage-labels", false, "label saved stage images with their stage names")
	set.BoolVar(&flags.compatVolumes, "compat-volumes", false, "discard RUN changes under declared image volumes")
	set.BoolVar(&flags.noHosts, "no-hosts", false, "preserve the image /etc/hosts during RUN")
	set.BoolVar(&flags.noHostname, "no-hostname", false, "preserve the image /etc/hostname during RUN")
	set.IntVarP(&flags.jobs, "jobs", "j", 1, "maximum concurrent build jobs (0 is unlimited)")
}

func (flags buildControlFlags) controls() (buildah.RunControls, error) {
	var cgroupManager, networkConfigDir string
	var configModules, cdiSpecDirs []string
	if flags.command != nil {
		set := flags.command.Flags()
		cgroupManager, _ = set.GetString("cgroup-manager")
		configModules, _ = set.GetStringArray("module")
		cdiSpecDirs, _ = set.GetStringArray("cdi-spec-dir")
		networkConfigDir, _ = set.GetString("network-config-dir")
	}
	if flags.cdiConfigDir != "" {
		cdiSpecDirs = []string{flags.cdiConfigDir}
	}
	return buildah.ParseRunControls(buildah.RunControlInput{
		HTTPProxy: flags.httpProxy, DNSServers: flags.dns, DNSSearch: flags.dnsSearch, DNSOptions: flags.dnsOption,
		Memory: flags.memory, MemorySwap: flags.memorySwap, CPUPeriod: flags.cpuPeriod, CPUQuota: flags.cpuQuota,
		CPUShares: flags.cpuShares, ShmSize: flags.shmSize, Ulimits: flags.ulimits,
		Volumes: flags.volumes, CapAdd: flags.capAdd, CapDrop: flags.capDrop, SecurityOptions: flags.security,
		Devices: flags.devices, GroupAdd: flags.groupAdd, UserNS: flags.userNS,
		UIDMap: flags.uidMap, GIDMap: flags.gidMap, UIDMapUser: flags.mapUser, GIDMapGroup: flags.mapGroup,
		CgroupNS: flags.cgroupNS, CgroupParent: flags.cgroupParent, CPUSetCPUs: flags.cpusetCPUs,
		CPUSetMems: flags.cpusetMems, PIDNS: flags.pidNS, IPCNS: flags.ipcNS, UTSNS: flags.utsNS,
		HooksDirs:    flags.hooksDirs,
		RuntimeFlags: flags.runtimeFlags,
		Isolation:    flags.isolation, Runtime: flags.runtime,
		NoHosts: flags.noHosts, NoHostname: flags.noHostname, CgroupManager: cgroupManager,
		ConfigModules: configModules, CDISpecDirs: cdiSpecDirs, NetworkConfigDir: networkConfigDir,
	})
}

type registryFlags struct {
	authFile       string
	certDir        string
	tlsVerify      bool
	credentials    string
	retry          uint
	retryDelay     time.Duration
	decryptionKeys []string
}

func (flags *registryFlags) addTo(command *cobra.Command) {
	set := command.Flags()
	set.StringVar(&flags.authFile, "authfile", "", "path to a containers-auth.json or Docker config.json registry credential file")
	set.StringVar(&flags.certDir, "cert-dir", "", "directory containing ca.crt and optional client certificates")
	set.StringVar(&flags.credentials, "creds", "", "registry username[:password] (overrides authfile)")
	set.UintVar(&flags.retry, "retry", 3, "number of registry retries after the first attempt")
	set.DurationVar(&flags.retryDelay, "retry-delay", 0, "delay between registry retries (default: native exponential backoff)")
	set.BoolVar(&flags.tlsVerify, "tls-verify", true, "require valid HTTPS certificates for registry connections")
}

func (flags registryFlags) tlsPolicy(command *cobra.Command) *bool {
	if !command.Flags().Changed("tls-verify") {
		return nil
	}
	return &flags.tlsVerify
}

func (flags registryFlags) transferOptions(command *cobra.Command) transfer.Options {
	return transfer.Options{
		AuthFile: flags.authFile, CertDir: flags.certDir, TLSVerify: flags.tlsPolicy(command),
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
	return buildah.LifecycleControls{NoLayers: !flags.layers, KeepIntermediate: !flags.remove && !flags.forceRemove, KeepFailed: !flags.forceRemove, BuildUnusedStages: !flags.skipUnused, CompatVolumes: flags.compatVolumes, SaveStages: flags.saveStages, StageLabels: flags.stageLabels}
}

func commandSignaturePolicy(cmd *cobra.Command) string {
	value, _ := cmd.Flags().GetString("signature-policy")
	return value
}
