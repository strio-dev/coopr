package buildah

import "testing"

func TestLifecycleContainerRetention(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		controls                 LifecycleControls
		failed, canceled, remove bool
	}{
		{"success-default", LifecycleControls{}, false, false, true},
		{"success-retained", LifecycleControls{KeepIntermediate: true}, false, false, false},
		{"failure-default", LifecycleControls{}, true, false, true},
		{"failure-retained", LifecycleControls{KeepFailed: true}, true, false, false},
		{"cancel-retained", LifecycleControls{KeepFailed: true}, true, true, false},
		{"no-layers-success", LifecycleControls{NoLayers: true, KeepIntermediate: true}, false, false, true},
		{"no-layers-failure", LifecycleControls{NoLayers: true, KeepFailed: true}, true, false, true},
		{"no-layers-cancel", LifecycleControls{NoLayers: true, KeepFailed: true}, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.controls.removeBuilder(tc.failed, tc.canceled); got != tc.remove {
				t.Fatalf("removeBuilder=%v want %v", got, tc.remove)
			}
		})
	}
}
