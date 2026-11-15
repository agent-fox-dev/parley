package config

import "time"

// Changes holds the keys of [changes] the loader owns.
type Changes struct {
	// MergeableDebounce is the shortest time between the starts of two
	// mergeability checks of one change; zero for none.
	MergeableDebounce time.Duration
}

// changesKeys are the keys of [changes] the loader owns. The section stays
// open to keys other packages register. The duration form has no sign, so
// a negative debounce is refused with any other value that is not one.
var changesKeys = []Key{
	{Section: "changes", Name: "mergeable_debounce", Type: String, Default: "5s", Validate: func(v any) error {
		_, err := durationValue(v)
		return err
	}},
}

// changesFrom returns the [changes] values of checked values.
func changesFrom(values map[string]map[string]any) Changes {
	// Each value is checked by its key's rule or is the key's default.
	debounce, _ := durationValue(values["changes"]["mergeable_debounce"])
	return Changes{MergeableDebounce: debounce}
}
