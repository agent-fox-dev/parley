package change

// The store-level moves of a change and the read of its versions, which
// the tests use before the setters and routes that wrap them.
var (
	AbandonChange = abandonChange
	LandChange    = landChange
	ReadVersions  = readVersions
)
