package buildah

// LifecycleControls selects execution and intermediate-container behavior.
// Runtime helpers always stop; confirmed failed build containers may be retained.
type LifecycleControls struct {
	NoLayers          bool `json:"no_layers,omitempty"`
	KeepIntermediate  bool `json:"keep_intermediate,omitempty"`
	KeepFailed        bool `json:"keep_failed,omitempty"`
	BuildUnusedStages bool `json:"build_unused_stages,omitempty"`
	CompatVolumes     bool `json:"compat_volumes,omitempty"`
}

func (controls LifecycleControls) removeBuilder(failed, canceled bool) bool {
	if controls.NoLayers {
		return true
	}
	if failed || canceled {
		return !controls.KeepFailed
	}
	return !controls.KeepIntermediate
}

func withoutLinkedLayers(operation Operation) Operation {
	switch value := operation.(type) {
	case Copy:
		value.Link = false
		return value
	case Add:
		value.Link = false
		return value
	case copyFromImageOperation:
		value.options.Link = false
		return value
	default:
		return operation
	}
}
