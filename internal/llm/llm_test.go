// Package llm_test covers the response-shape tolerances every model parser in
// this repo depends on. These are deliberately not tested through a client:
// they are pure string functions, and the behaviours they encode were each
// discovered from a real model response rather than reasoned about up front.
package llm_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/llm"
)

func TestStripJSONFence(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "unfenced passes through", raw: `[{"a":1}]`, want: `[{"a":1}]`},
		{
			// The exact shape observed from a live litellm-fronted Claude model
			// for a prompt that forbade fences.
			name: "json-tagged fence",
			raw:  "```json\n[]\n```",
			want: "[]",
		},
		{name: "bare fence", raw: "```\n[1,2]\n```", want: "[1,2]"},
		{
			// No newline means no payload to extract; the caller must report
			// the original text rather than a plausible-looking fabrication.
			name: "fence with no newline is returned unchanged",
			raw:  "```json",
			want: "```json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, llm.StripJSONFence(tt.raw))
		})
	}
}

// TestJSONArrayPayload_WrapsALoneObject is the regression test for a live
// failure: asked to cluster a batch that contained exactly one story, the model
// returned a bare object instead of a one-element array, and the pass died with
//
//	json: cannot unmarshal object into Go value of type []correlator.clusterResponseItem
//
// Same class as StripJSONFence's markdown fences — a property of the interface
// rather than a prompt bug — so the parsers absorb it instead of failing a whole
// pass over punctuation. Dropping the response would be worse than the error
// was: it is a real narrative the model correctly identified.
func TestJSONArrayPayload_WrapsALoneObject(t *testing.T) {
	raw := `{"kind":"extends","narrative_id":9,"title":"t","summary":"s","event_indices":[0]}`

	var items []map[string]any
	require.NoError(t, json.Unmarshal([]byte(llm.JSONArrayPayload(raw)), &items),
		"a lone object must parse as a one-element array")
	require.Len(t, items, 1)
	assert.Equal(t, "extends", items[0]["kind"])
}

func TestJSONArrayPayload_LeavesAnArrayAlone(t *testing.T) {
	raw := `[{"kind":"new"},{"kind":"extends"}]`

	var items []map[string]any
	require.NoError(t, json.Unmarshal([]byte(llm.JSONArrayPayload(raw)), &items))
	assert.Len(t, items, 2, "an array must pass through untouched, not be nested")
}

func TestJSONArrayPayload_StripsAFenceFirst(t *testing.T) {
	// Both tolerances at once: a fenced lone object. Observed separately, and
	// nothing stops a model doing both in one reply.
	raw := "```json\n{\"kind\":\"new\",\"title\":\"t\"}\n```"

	var items []map[string]any
	require.NoError(t, json.Unmarshal([]byte(llm.JSONArrayPayload(raw)), &items))
	require.Len(t, items, 1)
	assert.Equal(t, "new", items[0]["kind"])
}

func TestJSONArrayPayload_LeavesMalformedInputAlone(t *testing.T) {
	// Tolerance stops at shape. Malformed JSON must still reach the caller's
	// json.Unmarshal and produce a loud error naming the raw response — never
	// be silently repaired into something that parses.
	for _, raw := range []string{
		`{"kind":"new"`,   // truncated object
		`[{"kind":"new"`,  // truncated array
		`not json at all`, // prose
		``,                // empty
		`   `,             // whitespace only
	} {
		var items []map[string]any
		assert.Error(t, json.Unmarshal([]byte(llm.JSONArrayPayload(raw)), &items),
			"malformed input %q must stay malformed", raw)
	}
}

func TestJSONArrayPayload_DoesNotWrapAScalar(t *testing.T) {
	// Only an object gets wrapped. A scalar is not a plausible one-element
	// response — it is a broken one, and must surface as such rather than
	// becoming a valid-looking [42].
	for _, raw := range []string{`42`, `"a string"`, `true`} {
		var items []map[string]any
		assert.Error(t, json.Unmarshal([]byte(llm.JSONArrayPayload(raw)), &items),
			"scalar %q must not be wrapped into a plausible-looking array", raw)
	}
}

// TestJSONArrayPayload_NullYieldsNoItems documents a case JSON itself decides,
// not this function: `null` unmarshals into a nil slice with no error. Asserted
// so the behaviour is deliberate rather than discovered later — a model
// answering `null` means "nothing to report", and an empty result is the honest
// reading of that. It is emphatically NOT wrapped into `[null]`, which would
// become a phantom one-element result.
func TestJSONArrayPayload_NullYieldsNoItems(t *testing.T) {
	var items []map[string]any
	require.NoError(t, json.Unmarshal([]byte(llm.JSONArrayPayload(`null`)), &items))
	assert.Empty(t, items, "null must read as nothing to report, never as one phantom item")
}
