package buildah

import (
	"time"

	"github.com/strio-dev/coopr/internal/buildcontext"
	"go.podman.io/buildah/pkg/sourcepolicy"
)

// workerExecutionOptions is the explicit shared allowlist for same-binary
// private workers. Process-local callbacks, resolvers and SystemContext stay
// outside this record and are reconstructed by each worker's caller.
type workerExecutionOptions struct {
	StreamProgress     bool                 `json:"stream_progress,omitempty"`
	ExecutableOrigin   string               `json:"executable_origin,omitempty"`
	ProgressReference  string               `json:"progress_reference,omitempty"`
	ProgressPrefix     string               `json:"progress_prefix,omitempty"`
	Lifecycle          LifecycleControls    `json:"lifecycle,omitempty"`
	Store              StoreOptions         `json:"store"`
	TransientRunMounts []RunMount           `json:"transient_run_mounts,omitempty"`
	ContextDir         string               `json:"context_dir,omitempty"`
	SourcePolicyFile   string               `json:"source_policy_file,omitempty"`
	SourcePolicy       *sourcepolicy.Policy `json:"source_policy,omitempty"`
	IgnoreFile         string               `json:"ignore_file,omitempty"`
	ContextArtifacts   []string             `json:"context_artifacts,omitempty"`
	Isolation          string               `json:"isolation,omitempty"`
	Runtime            string               `json:"runtime,omitempty"`
	CacheLocalDir      string               `json:"cache_local_dir,omitempty"`
	CacheMountDir      string               `json:"cache_mount_dir,omitempty"`
	CacheRepository    string               `json:"cache_repository,omitempty"`
	CacheFrom          []CacheSpec          `json:"cache_from,omitempty"`
	CacheTo            []CacheSpec          `json:"cache_to,omitempty"`
	NoCache            bool                 `json:"no_cache,omitempty"`
	Network            string               `json:"network,omitempty"`
	AddHosts           []string             `json:"add_hosts,omitempty"`
	RunControls        RunControls          `json:"run_controls,omitempty"`
	ImageControls      ImageControls        `json:"image_controls,omitempty"`
	Timestamp          *int64               `json:"timestamp,omitempty"`
	SourceDateEpoch    *int64               `json:"source_date_epoch,omitempty"`
	CacheTTL           *time.Duration       `json:"cache_ttl,omitempty"`
	Jobs               int                  `json:"jobs,omitempty"`
	LogRusage          bool                 `json:"log_rusage,omitempty"`
	RusageLogFile      string               `json:"rusage_log_file,omitempty"`
	RewriteTimestamp   bool                 `json:"rewrite_timestamp,omitempty"`
	Allow              []string             `json:"allow,omitempty"`
	BuildContexts      []buildcontext.Spec  `json:"build_contexts,omitempty"`
	Secrets            []string             `json:"secrets,omitempty"`
	SSH                []string             `json:"ssh,omitempty"`
	ProxyArgs          map[string]string    `json:"proxy_args,omitempty"`
	AuthFile           string               `json:"auth_file,omitempty"`
	CertDir            string               `json:"cert_dir,omitempty"`
	TLSVerify          *bool                `json:"tls_verify,omitempty"`
	PullPolicy         string               `json:"pull_policy,omitempty"`
}

func (options workerExecutionOptions) planOptions() PlanOptions {
	return PlanOptions{
		executableOrigin:        options.ExecutableOrigin,
		ProgressReference:       options.ProgressReference,
		ProgressPrefix:          options.ProgressPrefix,
		Lifecycle:               options.Lifecycle,
		Store:                   options.Store,
		TransientRunMounts:      options.TransientRunMounts,
		ContextDir:              options.ContextDir,
		SourcePolicyFile:        options.SourcePolicyFile,
		SourcePolicy:            options.SourcePolicy,
		IgnoreFile:              options.IgnoreFile,
		ContextArtifacts:        options.ContextArtifacts,
		Isolation:               options.Isolation,
		Runtime:                 options.Runtime,
		CacheLocalDir:           options.CacheLocalDir,
		CacheMountDir:           options.CacheMountDir,
		CacheRepository:         options.CacheRepository,
		CacheFrom:               options.CacheFrom,
		CacheTo:                 options.CacheTo,
		NoCache:                 options.NoCache,
		Network:                 options.Network,
		AddHosts:                options.AddHosts,
		RunControls:             options.RunControls,
		ImageControls:           options.ImageControls,
		Timestamp:               options.Timestamp,
		SourceDateEpochOverride: options.SourceDateEpoch,
		CacheTTL:                options.CacheTTL,
		Jobs:                    options.Jobs,
		LogRusage:               options.LogRusage,
		RusageLogFile:           options.RusageLogFile,
		RewriteTimestamp:        options.RewriteTimestamp,
		Allow:                   options.Allow,
		BuildContexts:           options.BuildContexts,
		Secrets:                 options.Secrets,
		SSH:                     options.SSH,
		ProxyArgs:               options.ProxyArgs,
	}
}
