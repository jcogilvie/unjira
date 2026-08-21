// Package credentials holds the shape of a remote system's credentials, keyed
// by the config connection they belong to.
//
// It exists as its own package because collectors need to name the type and
// cmd/unjira is package main (unimportable), while internal/config would be
// the wrong home: credentials come from the environment, never from config
// files, and keeping them out of the config package keeps that rule visible
// in the layout rather than only in a doc comment.
//
// Parsing lives in cmd/unjira, where Kong decodes UNJIRA_JIRA_CREDENTIALS from
// a single JSON-object env var. This package is only the agreed shape.
package credentials

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
