// Package credentials_test covers the lookup contract collectors rely on.
package credentials_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/credentials"
)

func TestJSONSet_UnmarshalsTwoConnectionObject(t *testing.T) {
	var set credentials.JSONSet

	err := set.UnmarshalJSON([]byte(
		`{"corp":{"email":"a@x.com","token":"tok1"},"paas":{"email":"b@x.com","token":"tok2"}}`,
	))

	require.NoError(t, err)

	corp, ok := set.Set().For("corp")
	require.True(t, ok)
	assert.Equal(t, "a@x.com", corp.Email)
	assert.Equal(t, "tok1", corp.Token)

	paas, ok := set.Set().For("paas")
	require.True(t, ok)
	assert.Equal(t, "b@x.com", paas.Email)
	assert.Equal(t, "tok2", paas.Token)
}

func TestJSONSet_MalformedJSONErrors(t *testing.T) {
	var set credentials.JSONSet

	err := set.UnmarshalJSON([]byte(`not json`))

	require.Error(t, err)
}

func TestFromEnv_UnsetReportsNotFoundWithoutError(t *testing.T) {
	// t.Setenv to "" (rather than os.Unsetenv, which t.Cleanup cannot restore
	// symmetrically) is equivalent for FromEnv's purposes: it treats an empty
	// var the same as an absent one, and this keeps the test parallel-safe and
	// self-restoring.
	t.Setenv(credentials.EnvVar, "")

	set, found, err := credentials.FromEnv()

	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, credentials.Set{}, set)
}

func TestFromEnv_ValidVarResolvesConnection(t *testing.T) {
	t.Setenv(credentials.EnvVar, `{"dev":{"email":"dev@x.com","token":"dev-tok"}}`)

	set, found, err := credentials.FromEnv()

	require.NoError(t, err)
	assert.True(t, found)

	dev, ok := set.For("dev")
	require.True(t, ok)
	assert.Equal(t, "dev@x.com", dev.Email)
	assert.Equal(t, "dev-tok", dev.Token)
}

func TestFromEnv_MalformedVarErrors(t *testing.T) {
	t.Setenv(credentials.EnvVar, `not json`)

	_, _, err := credentials.FromEnv()

	require.Error(t, err)
}

func TestSet_ForReturnsCredentialByConnectionName(t *testing.T) {
	set := credentials.NewSet(map[string]credentials.Credential{
		"corp": {Email: "me@corp.example", Token: "t1"},
		"paas": {Email: "me@paas.example", Token: "t2"},
	})

	got, ok := set.For("corp")

	require.True(t, ok)
	assert.Equal(t, "me@corp.example", got.Email)
	assert.Equal(t, "t1", got.Token)
}

func TestSet_ForMissingConnectionReportsNotFound(t *testing.T) {
	set := credentials.NewSet(map[string]credentials.Credential{})

	_, ok := set.For("nope")

	assert.False(t, ok, "a missing connection must be reported, not returned as a zero credential")
}

func TestSet_ZeroValueIsUsable(t *testing.T) {
	// A Set nobody populated must not panic on lookup: a collector that needs
	// no credentials is given the zero value.
	var set credentials.Set

	_, ok := set.For("corp")

	assert.False(t, ok)
}
