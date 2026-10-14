package definition

import (
	"math/big"
	"testing"

	kdl "github.com/calico32/kdl-go"
)

func TestNormalizePermissionUsesSignedOctalLiteral(t *testing.T) {
	// Public values isolate Coopr normalization from the pinned parser's rejection
	// of signed base-prefixed literals. They do not add alternate source syntax.
	for _, test := range []struct {
		literal    string
		value      int
		permission bool
		want       string
	}{
		{"0o644", 420, true, "0644"},
		{"+0o644", 420, true, "0644"},
		{"-0o644", -420, true, "-0644"},
		{"+0o644", 420, false, "420"},
		{"0x1a4", 420, true, "420"},
		{"420", 420, true, "420"},
	} {
		for _, value := range []kdl.Value{kdl.NewInt(test.value), kdl.NewBigInt(big.NewInt(int64(test.value)))} {
			got, err := normalizeScalar(value.WithLiteral(test.literal), test.permission)
			if err != nil || got != test.want {
				t.Errorf("normalize %q (permission=%t): %q, %v, want %q", test.literal, test.permission, got, err, test.want)
			}
		}
	}
}
