package sandbox

// The OpenShell releases the hub works with: from LowestSupportedRelease,
// included, to FirstUnsupportedRelease, excluded, compared as semantic
// versions. They follow the OpenShell SDK in go.mod
// (github.com/ai-openshell/sdk-go v0.1.8): the lowest is that version's
// major.minor.0, and the first unsupported is the next major version, or
// the next minor version while the major version is 0. Moving the SDK to
// another minor version moves them.
const (
	LowestSupportedRelease  = "0.1.0"
	FirstUnsupportedRelease = "0.2.0"
)
