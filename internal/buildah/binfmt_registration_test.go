package buildah

import "testing"

func TestBinfmtFixBinaryEnabled(t *testing.T) {
	for _, test := range []struct {
		name         string
		registration string
		want         bool
	}{
		{"fix-binary", "enabled\ninterpreter /nix/store/qemu\nflags: F\n", true},
		{"multiple-flags", "enabled\nflags: OPF\n", true},
		{"reversed-flags", "enabled\nflags: FPO\n", true},
		{"disabled", "disabled\nflags: F\n", false},
		{"missing-fix-binary", "enabled\nflags: OP\n", false},
		{"missing-flags", "enabled\ninterpreter /some/path/F\n", false},
		{"enabled-in-path", "disabled\ninterpreter /enabled/qemu\nflags: F\n", false},
		{"empty", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := binfmtFixBinaryEnabled(test.registration); got != test.want {
				t.Fatalf("registration %q: enabled with F = %t, want %t", test.registration, got, test.want)
			}
		})
	}
}
