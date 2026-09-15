package logging_test

// logging_test.go covers the logging foundation added for finding F17.
//
// Two requirements shaped it, and they pull in opposite directions:
//
//   - unjira is a CLI today, so the default has to be readable by a person at a
//     terminal. Text.
//   - it is expected to run as a cloud service across an org, so structured output is
//     a FIRST-CLASS mode rather than a debug affordance. JSON, selectable, and carrying
//     the same attributes the text handler shows.
//
// The tests below assert both shapes because "we can add JSON later" is how a log
// consumer ends up parsing a human-readable format with regexes.

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/logging"
)

func TestNew_TextIsTheDefaultShape(t *testing.T) {
	var buf bytes.Buffer

	log, err := logging.New(logging.Options{Format: "text", Level: "info", Out: &buf})
	require.NoError(t, err)

	log.Info("clustering", "candidates", 16, "context_narratives", 52)

	got := buf.String()
	assert.Contains(t, got, "clustering")
	assert.Contains(t, got, "candidates=16",
		"text output must carry the attributes, not just the message: the numbers ARE the "+
			"observation F17 is about")
	assert.Contains(t, got, "context_narratives=52")
	assert.False(t, json.Valid(bytes.TrimSpace(buf.Bytes())),
		"the default must be the human shape, not JSON that happens to be readable")
}

func TestNew_JSONCarriesTheSameAttributes(t *testing.T) {
	var buf bytes.Buffer

	log, err := logging.New(logging.Options{Format: "json", Level: "info", Out: &buf})
	require.NoError(t, err)

	log.Info("clustering", "candidates", 16, "context_narratives", 52)

	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got),
		"JSON mode must emit one parseable object per line, or a log shipper cannot consume it")

	assert.Equal(t, "clustering", got["msg"])
	assert.InDelta(t, 16, got["candidates"], 0)
	assert.InDelta(t, 52, got["context_narratives"], 0)
	assert.Contains(t, got, "time", "a shipped event needs its own timestamp, not the reader's")
	assert.Equal(t, "INFO", got["level"])
}

// TestNew_LevelFiltersBelowThreshold is the half that makes this usable under `watch`.
// F17's complaint was silence, but the opposite failure is a per-narrative line on an
// interval loop; a level is what lets one binary serve both.
func TestNew_LevelFiltersBelowThreshold(t *testing.T) {
	var buf bytes.Buffer

	log, err := logging.New(logging.Options{Format: "text", Level: "warn", Out: &buf})
	require.NoError(t, err)

	log.Info("this is below the threshold")
	log.Warn("this is at it")

	got := buf.String()
	assert.NotContains(t, got, "below the threshold")
	assert.Contains(t, got, "at it")
}

// TestNew_DebugIsReachable pins the diagnostic end. The whole point of a level is that
// the detail exists in the binary and is off by default, rather than not existing.
func TestNew_DebugIsReachable(t *testing.T) {
	var buf bytes.Buffer

	log, err := logging.New(logging.Options{Format: "text", Level: "debug", Out: &buf})
	require.NoError(t, err)

	log.Debug("prompt built", "est_tokens", 104219)

	assert.Contains(t, buf.String(), "est_tokens=104219")
}

// TestNew_RejectsAnUnknownFormat prefers an error to a silent fallback. A deployment
// that asked for JSON and got text would have its log pipeline break at the consumer,
// far from the typo.
func TestNew_RejectsAnUnknownFormat(t *testing.T) {
	_, err := logging.New(logging.Options{Format: "yaml", Level: "info"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "yaml",
		"the message must name the rejected value, since the fix is to change it")
	assert.Contains(t, err.Error(), "json", "and name what is available")
}

// TestNew_RejectsAnUnknownLevel is the same argument. Defaulting a misspelled level to
// info would silently discard the debug output someone was trying to enable.
func TestNew_RejectsAnUnknownLevel(t *testing.T) {
	_, err := logging.New(logging.Options{Format: "text", Level: "verbose"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "verbose")
}

// TestNew_DefaultsAreUsableWhenUnset keeps construction from being a config exercise.
// A zero Options must produce a working logger, because the alternative is every test
// and every caller spelling out the same two strings.
func TestNew_DefaultsAreUsableWhenUnset(t *testing.T) {
	var buf bytes.Buffer

	log, err := logging.New(logging.Options{Out: &buf})
	require.NoError(t, err)

	log.Info("works")
	log.Debug("filtered by the default level")

	got := buf.String()
	assert.Contains(t, got, "works")
	assert.NotContains(t, got, "filtered",
		"the default level must be info: debug on by default is how a log becomes unreadable")
}

// TestDiscard is what keeps a logger from being a required argument everywhere. Library
// code takes a *slog.Logger and must never nil-check it, so there has to be a working
// no-op to hand it.
func TestDiscard(t *testing.T) {
	log := logging.Discard()
	require.NotNil(t, log)

	// The assertion is that this does not panic and writes nowhere observable.
	log.Info("goes nowhere", "attr", 1)
	log.Error("nor does this")
}

// TestFor_TagsTheComponent pins the convention that replaces the message prefixes.
// Every existing log line began with its component ("pipeline: ", "correlator: "),
// which is an attribute wearing a costume — unqueryable in JSON and repeated in every
// format string.
func TestFor_TagsTheComponent(t *testing.T) {
	var buf bytes.Buffer

	base, err := logging.New(logging.Options{Format: "json", Level: "info", Out: &buf})
	require.NoError(t, err)

	logging.For(base, "correlator").Info("compacted narrative", "narrative_id", 42)

	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))

	assert.Equal(t, "correlator", got["component"],
		"the component belongs in a field a shipper can filter on, not in the message text")
	assert.Equal(t, "compacted narrative", got["msg"],
		"and the message keeps only what varies")
	assert.NotContains(t, got["msg"], "correlator:")
}

// TestFor_ToleratesANilLogger removes the last reason for a call site to branch. A
// package handed no logger should still be able to tag itself.
func TestFor_ToleratesANilLogger(t *testing.T) {
	log := logging.For(nil, "correlator")
	require.NotNil(t, log)

	log.Info("must not panic")
}

// TestNew_WritesToTheProvidedWriter is why Out exists at all: without it these tests
// would have to capture os.Stderr, and a cloud deployment could not redirect.
func TestNew_WritesToTheProvidedWriter(t *testing.T) {
	var buf bytes.Buffer

	log, err := logging.New(logging.Options{Out: &buf})
	require.NoError(t, err)

	log.Info("captured")

	assert.Contains(t, buf.String(), "captured")
}
