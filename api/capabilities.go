package api

// Capabilities identify supported operations and optional request features.
// They are negotiated independently of the executable's version string.
const (
	CapabilityRunNetwork        = "run-network"
	CapabilityExecute           = "execute"
	CapabilityComponentLoad     = "component-load"
	CapabilityComponentLoadName = "component-load-name"
	CapabilityBuild             = "build"
	CapabilityBuildPlatform     = "build-platform"
	CapabilityComponent         = "component"
	CapabilityPublish           = "publish"
	CapabilityPublishOCILayout  = "publish-oci-layout"
	CapabilityRegistryCache     = "registry-cache"
	CapabilityOperationProgress = "operation-progress"
	CapabilityNamedContexts     = "named-contexts"
	CapabilityRunMounts         = "run-mounts"
)
