// Package credentials holds the shape of a remote system's credentials, keyed
// by the config connection they belong to, and the one decoder every caller
// shares.
//
// It exists as its own package because collectors need to name the type and
// cmd/unjira is package main (unimportable), while internal/config would be
// the wrong home: credentials come from the environment, never from config
// files, and keeping them out of the config package keeps that rule visible
// in the layout rather than only in a doc comment.
//
// There are two callers of the decoder, with different entry points into the
// same JSON: cmd/unjira is Kong-driven, so it wants a json.Unmarshaler type it
// can tag onto a CLI field (JSONSet); internal/live is not Kong-driven at all
// — its tests call FromEnv directly. Both go through JSONSet.UnmarshalJSON so
// there is exactly one parser.
package credentials

import (
	"encoding/json"
	"fmt"
	"os"
)

// EnvVar is the environment variable every credential form is read from: a
// single JSON object mapping each configured Jira connection's name to its
// {email, token} pair, e.g.:
//
//	UNJIRA_JIRA_CREDENTIALS='{"corp":{"email":"a@x.com","token":"..."},"paas":{"email":"b@x.com","token":"..."}}'
//
// One var per credential kind, not one pair per connection — this scales to
// any number of configured connections without a new env var per one.
const EnvVar = "UNJIRA_JIRA_CREDENTIALS"

// Credential is one connection's email/token pair.
type Credential struct {
	Email string `json:"email"`
	Token string `json:"token"`
}

// Set maps a config connection name (config.JiraConnection.Name) to its
// credential. The zero value is usable and reports every lookup as missing,
// so a collector needing no credentials can be handed one safely.
type Set struct {
	byName map[string]Credential
}

// NewSet returns a Set over byName. The map is used as given, not copied:
// callers build it once at startup and do not mutate it afterward.
func NewSet(byName map[string]Credential) Set {
	return Set{byName: byName}
}

// For returns the credential for a connection name, reporting whether one was
// configured. Missing is a distinct outcome from an empty credential: a
// caller must be able to say "no credentials for connection X" rather than
// failing later with an unauthenticated request.
func (s Set) For(connectionName string) (Credential, bool) {
	credential, ok := s.byName[connectionName]

	return credential, ok
}

// JSONSet decodes EnvVar's JSON object into a Set.
//
// Must be a named struct wrapping the map (not a bare map[string]T): Kong
// checks for a json.Unmarshaler implementation by type before falling back to
// its own built-in map decoder (which expects key=value;key2=value2 syntax,
// not JSON), and a bare map type never satisfies json.Unmarshaler. Whatever
// CLI field is tagged with EnvVar must be of this type (or embed it) for
// Kong's env decoding to see JSON at all.
type JSONSet struct {
	set Set
}

// UnmarshalJSON implements json.Unmarshaler so Kong decodes this type from
// its env var automatically, and so FromEnv can share the same code path.
func (j *JSONSet) UnmarshalJSON(data []byte) error {
	var byName map[string]Credential
	if err := json.Unmarshal(data, &byName); err != nil {
		return err
	}

	j.set = NewSet(byName)

	return nil
}

// Set returns the decoded credentials.
func (j JSONSet) Set() Set {
	return j.set
}

// FromEnv reads and decodes EnvVar for callers that are not Kong-driven (the
// live test tier). found distinguishes "not configured" (the var is unset or
// empty — a caller should skip, not fail) from a var that is set but does not
// parse (a caller should fail loudly: see the package's "never silently drop
// data" convention in docs/design-notes.md — masking a malformed credential
// blob as "not configured" would turn a misconfiguration into a confusing
// downstream "no credentials for connection X" error instead).
//
// Loading .env is left to the caller (envfile.Load), not done here: FromEnv
// only decodes whatever is already in the environment, so its behavior does
// not depend on the caller's working directory the way envfile's root-finding
// walk does.
func FromEnv() (set Set, found bool, err error) {
	raw := os.Getenv(EnvVar)
	if raw == "" {
		return Set{}, false, nil
	}

	var decoded JSONSet
	if err := decoded.UnmarshalJSON([]byte(raw)); err != nil {
		return Set{}, true, fmt.Errorf("decoding %s: %w", EnvVar, err)
	}

	return decoded.Set(), true, nil
}
