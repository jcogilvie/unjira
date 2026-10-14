package claudecode_test

// ledger_test.go covers the token ledger rows Collect writes, observed through the store.

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/collector/claudecode"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/store/storetest"
)

const model = "claude-opus-4-1"

func usage(input, output, cacheWrite, cacheRead int) map[string]any {
	return map[string]any{
		"input_tokens":                input,
		"output_tokens":               output,
		"cache_creation_input_tokens": cacheWrite,
		"cache_read_input_tokens":     cacheRead,
	}
}

func at(ts string) time.Time {
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		panic(err)
	}

	return parsed
}

// openLedgerStore opens a fresh store and returns it with its path, so the test can read
// token_messages directly: the store has no reader for it yet.
func openLedgerStore(t *testing.T) (*store.Store, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "db.sqlite")
	s, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s, path
}

// rawDB opens a second connection to the store's file, with the store's time options.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+path+"?_time_format=sqlite&_timezone=UTC")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	return db
}

func ledger(t *testing.T, path string) []store.MessageUsage {
	t.Helper()

	rows, err := rawDB(t, path).Query(
		`SELECT message_id, transcript_id, COALESCE(agent_id, ''), occurred_at, model,
		        input_tokens, output_tokens, cache_write_tokens, cache_read_tokens,
		        COALESCE(covering_external_id, '')
		 FROM token_messages ORDER BY message_id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var out []store.MessageUsage
	for rows.Next() {
		var r store.MessageUsage
		require.NoError(t, rows.Scan(&r.MessageID, &r.TranscriptID, &r.AgentID, &r.OccurredAt, &r.Model,
			&r.InputTokens, &r.OutputTokens, &r.CacheWriteTokens, &r.CacheReadTokens, &r.CoveringExternalID))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())

	return out
}

func diffLedger(t *testing.T, want []store.MessageUsage, path string) {
	t.Helper()

	got := ledger(t, path)
	if diff := cmp.Diff(want, got, cmp.Comparer(func(a, b time.Time) bool { return a.Equal(b) })); diff != "" {
		t.Errorf("token_messages mismatch (-want +got):\n%s", diff)
	}
}

// threeAsks is a root transcript with enough user messages to emit its segment event.
func threeAsks(t *testing.T) *transcriptBuilder {
	t.Helper()

	return newTranscript(t).
		User("2026-09-01T10:00:00Z", "one").
		User("2026-09-01T10:01:00Z", "two").
		User("2026-09-01T10:02:00Z", "three")
}

func TestCollect_LedgersOneRowPerMessageID(t *testing.T) {
	root := t.TempDir()
	s, dbPath := openLedgerStore(t)
	b := threeAsks(t).
		Assistant("2026-09-01T10:03:00Z", "msg_1", model, usage(5, 7, 11, 13)).
		Assistant("2026-09-01T10:03:01Z", "msg_1", model, usage(5, 7, 11, 13)).
		Assistant("2026-09-01T10:03:02Z", "msg_1", model, usage(5, 7, 11, 13))
	path := b.WriteRoot(root, slug, "s1")

	collectWith(t, s, root, nil)
	collectWith(t, s, root, nil)
	b.User("2026-09-01T11:00:00Z", "four").WriteRoot(root, slug, "s1")
	// A later mtime, so the grown file's cursor position differs on any filesystem.
	later := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(path, later, later))
	got := segmentEvents(collectWith(t, s, root, nil))

	require.Len(t, got, 1)
	stat, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, "s1:"+strconv.FormatInt(stat.Size(), 10)+":0", got[0].ExternalID, "the newest snapshot's segment")
	diffLedger(t, []store.MessageUsage{
		storetest.MessageUsage("msg_1").Transcript("s1", "").At(at("2026-09-01T10:03:00Z")).
			Model(model).Tokens(5, 7, 11, 13).Covering(got[0].ExternalID).Build(),
	}, dbPath)
}

func TestCollect_LedgerTakesMaxAcrossLines(t *testing.T) {
	root := t.TempDir()
	s, dbPath := openLedgerStore(t)
	threeAsks(t).
		Assistant("2026-09-01T10:03:00Z", "msg_1", model, usage(5, 3, 0, 0)).
		Assistant("2026-09-01T10:03:01Z", "msg_1", model, usage(5, 120, 0, 0)).
		WriteRoot(root, slug, "s1")

	collectWith(t, s, root, nil)

	rows := ledger(t, dbPath)
	require.Len(t, rows, 1)
	assert.Equal(t, int64(120), rows[0].OutputTokens)
}

func TestCollect_SubagentRowsCarryAgentID(t *testing.T) {
	root := t.TempDir()
	s, dbPath := openLedgerStore(t)
	const agent = "a2fc2fb353c169993"
	subagentTranscript(t, agent).
		User("2026-09-20T10:01:00Z", "two").
		User("2026-09-20T10:02:00Z", "three").
		Assistant("2026-09-20T10:03:00Z", "msg_sub", model, usage(1, 2, 3, 4)).
		WriteSubagent(root, slug, parentSession, agent)

	collectWith(t, s, root, nil)

	rows := ledger(t, dbPath)
	require.Len(t, rows, 1)
	assert.Equal(t, "agent-"+agent, rows[0].TranscriptID)
	assert.Equal(t, agent, rows[0].AgentID)
	assert.True(t, strings.HasPrefix(rows[0].CoveringExternalID, "agent-"+agent+":"),
		"covering id %q names the subagent's own segment event", rows[0].CoveringExternalID)
}

// A segment that emits no event for want of user text still ledgers its spend, with no
// covering event.
func TestCollect_LedgersMessagesWithoutEvent(t *testing.T) {
	root := t.TempDir()
	s, dbPath := openLedgerStore(t)
	newTranscript(t).
		Assistant("2026-09-01T10:03:00Z", "msg_1", model, usage(5, 7, 0, 0)).
		WriteRoot(root, slug, "s1")

	got := collectWith(t, s, root, nil)

	assert.Empty(t, segmentEvents(got))
	diffLedger(t, []store.MessageUsage{
		storetest.MessageUsage("msg_1").Transcript("s1", "").At(at("2026-09-01T10:03:00Z")).
			Model(model).Tokens(5, 7, 0, 0).Build(),
	}, dbPath)
}

// An excluded cwd is not the user's work to account for, so it gets no rows at all.
func TestCollect_ExcludedCwdIsNotLedgered(t *testing.T) {
	root := t.TempDir()
	s, dbPath := openLedgerStore(t)
	repo := "/Users/j/workspace/unjira"
	threeAsks(t).InCwd(repo).
		Assistant("2026-09-01T10:03:00Z", "msg_1", model, usage(5, 7, 0, 0)).
		WriteRoot(root, slug, "s1")

	collectWith(t, s, root, map[string]any{"exclude_cwds": []string{repo}})

	assert.Empty(t, ledger(t, dbPath))
}

func TestCollect_SkipsSyntheticMessages(t *testing.T) {
	root := t.TempDir()
	s, dbPath := openLedgerStore(t)
	threeAsks(t).
		Assistant("2026-09-01T10:03:00Z", "msg_synthetic", "<synthetic>", usage(0, 0, 0, 0)).
		Assistant("2026-09-01T10:04:00Z", "msg_real", model, usage(5, 7, 0, 0)).
		WriteRoot(root, slug, "s1")

	collectWith(t, s, root, nil)

	rows := ledger(t, dbPath)
	require.Len(t, rows, 1)
	assert.Equal(t, "msg_real", rows[0].MessageID)
}

// A resumed copy's lines are ledgered only from the transcript that owns them (F33). The
// copy's name sorts before the original's, so if the copy ledgered the copied message it
// would take the row (the store's holder is the smallest (occurred_at, transcript_id)).
func TestCollect_ResumedCopyLedgersOnce(t *testing.T) {
	root := t.TempDir()
	s, dbPath := openLedgerStore(t)
	orig := originalSession(t).Assistant("2026-09-01T10:03:00Z", "msg_orig", model, usage(5, 7, 0, 0))
	orig.WriteRoot(root, slug, "z-orig")
	orig.Resumed("resumed").
		User("2026-09-02T09:00:00Z", "the resumed ask").
		User("2026-09-02T09:01:00Z", "five").
		User("2026-09-02T09:02:00Z", "six").
		Assistant("2026-09-02T09:03:00Z", "msg_resumed", model, usage(1, 2, 0, 0)).
		WriteRoot(root, slug, "a-resumed")

	collectWith(t, s, root, nil)

	rows := ledger(t, dbPath)
	require.Len(t, rows, 2)
	assert.Equal(t, "msg_orig", rows[0].MessageID)
	assert.Equal(t, "z-orig", rows[0].TranscriptID)
	assert.True(t, strings.HasPrefix(rows[0].CoveringExternalID, "z-orig:"), rows[0].CoveringExternalID)
	assert.Equal(t, "msg_resumed", rows[1].MessageID)
	assert.Equal(t, "a-resumed", rows[1].TranscriptID)
}

// A failed ledger write fails the pass before the cursor moves, so the next pass re-reads
// the transcript and its spend is not lost.
func TestCollect_LedgerWriteFailureLeavesCursor(t *testing.T) {
	root := t.TempDir()
	s, dbPath := openLedgerStore(t)
	path := threeAsks(t).
		Assistant("2026-09-01T10:03:00Z", "msg_1", model, usage(5, 7, 0, 0)).
		WriteRoot(root, slug, "s1")
	db := rawDB(t, dbPath)
	_, err := db.Exec(`CREATE TRIGGER refuse_ledger BEFORE INSERT ON token_messages
		BEGIN SELECT RAISE(ABORT, 'ledger refused'); END`)
	require.NoError(t, err)

	err = claudecode.New().Collect(
		pipeline.CollectContext{Store: s, Options: map[string]any{"transcript_root": root}},
		func(events.Event) {},
	)

	require.ErrorContains(t, err, "ledger refused")
	cursor, err := s.GetCursor(claudecode.Name, path)
	require.NoError(t, err)
	assert.Empty(t, cursor, "the cursor did not move")

	_, err = db.Exec(`DROP TRIGGER refuse_ledger`)
	require.NoError(t, err)
	collectWith(t, s, root, nil)
	assert.Len(t, ledger(t, dbPath), 1, "the next pass re-read the transcript")
}

// A negative counter fails the pass before the cursor moves, as any malformed counter does.
func TestCollect_NegativeCounterFailsBeforeCursor(t *testing.T) {
	root := t.TempDir()
	s, _ := openLedgerStore(t)
	path := threeAsks(t).
		Assistant("2026-09-01T10:03:00Z", "msg_1", model, usage(5, -7, 0, 0)).
		WriteRoot(root, slug, "s1")

	err := claudecode.New().Collect(
		pipeline.CollectContext{Store: s, Options: map[string]any{"transcript_root": root}},
		func(events.Event) {},
	)

	require.ErrorContains(t, err, `"output_tokens"`)
	cursor, err := s.GetCursor(claudecode.Name, path)
	require.NoError(t, err)
	assert.Empty(t, cursor, "the cursor did not move")
}
