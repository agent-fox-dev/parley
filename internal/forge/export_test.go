package forge

// The reduction's mapping contract, for tests that reduce with a mapping of
// their own.
var (
	ErrNoMapping    = errNoMapping
	ErrMissingField = errMissingField
	RequiredInt     = requiredInt
	RequiredString  = requiredString
)

// ReduceWith reduces a delivery as Reduce does, with the mapping m in place
// of the forge's own.
func ReduceWith(m func(event, action string, payload map[string]any) ([]HookEvent, error),
	kind ForgeKind, event string, body []byte, repos Repos) Reduction {
	return reduce(kind, event, body, repos, m)
}
