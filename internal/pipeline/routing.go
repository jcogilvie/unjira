package pipeline

import (
	"fmt"

	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// UnroutedKey is a stored issue key no configured tracker's scopes cover, with why.
type UnroutedKey struct {
	Key    string
	Reason string
}

// UnroutedStoredKeys reports every stored issue key (store.StoredIssueKeys) that no
// tracker's scopes cover: read only through the resolver's fallback, or not readable at
// all.
//
// Routing is a function of the key and config and is never stored, so a config change
// that drops a scope strands the keys stored under it. They are reported here and kept:
// deleting a link because config moved would lose a reviewer's history to an edit that
// might be undone. Locate opens no backend, so the report costs no API call.
func UnroutedStoredKeys(s *store.Store, r *tasktracker.Resolver) ([]UnroutedKey, error) {
	keys, err := s.StoredIssueKeys()
	if err != nil {
		return nil, err
	}

	var out []UnroutedKey

	for _, key := range keys {
		loc, err := r.Locate(key)

		switch {
		case err != nil:
			out = append(out, UnroutedKey{Key: key, Reason: err.Error()})
		case loc.Fallback:
			out = append(out, UnroutedKey{Key: key, Reason: fmt.Sprintf(
				"no configured tracker's scopes cover %q; read through tracker %q as a fallback, never written",
				loc.Scope, loc.Tracker)})
		}
	}

	return out, nil
}
