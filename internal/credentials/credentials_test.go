// Package credentials_test covers the lookup contract collectors rely on.
package credentials_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/credentials"
)

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
