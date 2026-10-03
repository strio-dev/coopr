package definition

import "strings"

// IsLocalComponentReference distinguishes authored context paths from OCI
// references after argument expansion. Only an explicit path prefix selects
// a local definition; extensions do not affect registry reference resolution.
func IsLocalComponentReference(reference string) bool {
	return strings.HasPrefix(reference, "./") || strings.HasPrefix(reference, "../") || strings.HasPrefix(reference, "/")
}
