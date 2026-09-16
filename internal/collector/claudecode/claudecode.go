// Package claudecode collects Claude Code session transcripts.
//
// Sessions live as JSONL under ~/.claude/projects/<project-slug>/<session-id>.jsonl
// and grow as the session continues. Each changed file yields one snapshot
// event (external ID includes the file size, so a session that grows
// produces a new snapshot; identical re-reads dedupe at insert).
//
// This collector is deliberately deterministic: it extracts metadata,
// ticket-key candidates, and the opening ask. Judging what the session
// meant is the correlator's job.
package claudecode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
)

// DefaultBackfillDays is how far back sessions are collected when no
// backfill_days option is set.
const DefaultBackfillDays = 14

// Name is this collector's registration name.
const Name = "claude_code"

// DefaultMinSegmentMessages is the floor below which a branch run folds into its
// neighbour rather than becoming its own event.
//
// Three, from measurement rather than taste. Raw branch-slicing over-splits: across 46
// multi-branch sessions, 30 produced more contiguous runs than distinct branches, and
// the worst produced 51 runs from 14 branches — at this floor it produces 6. The
// motivating case (session e951ef78, whose vp-feedback-refactor run must stay separate
// and dated 2026-07-09) is unaffected at 3 and wrongly merged at 5, so the value has
// headroom below the point where it starts destroying real boundaries.
//
// Configurable via the `min_segment_messages` option, but with a default that works:
// F9 is this repo's standing argument against a knob whose correct value the operator
// has to discover.
const DefaultMinSegmentMessages = 3

// lineTypeUser is the transcript line type carrying a human-authored message.
// Named because three call sites compare against it and a typo in any of them would
// silently produce a session with zero user messages, which reads as "empty transcript".
const lineTypeUser = "user"

// Collector scans Claude Code session transcripts for new work.
type Collector struct{}

// New returns a Collector.
func New() *Collector {
	return &Collector{}
}

// Name returns this collector's registration name.
func (c *Collector) Name() string {
	return Name
}

// Collect scans every session transcript under cc.Options["transcript_root"]
// (default ~/.claude/projects), calling visit for each new event and
// advancing the store's cursor for every file scanned, whether or not it
// yielded an event.
func (c *Collector) Collect(cc pipeline.CollectContext, visit func(events.Event)) error {
	root := stringOption(cc.Options, "transcript_root", "")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolving home directory: %w", err)
		}
		root = filepath.Join(home, ".claude", "projects")
	}

	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}

	backfillDays := intOption(cc.Options, "backfill_days", DefaultBackfillDays)
	minSegment := intOption(cc.Options, "min_segment_messages", DefaultMinSegmentMessages)
	horizon := time.Now().UTC().AddDate(0, 0, -backfillDays)

	excludeCwds := stringSliceOption(cc.Options, "exclude_cwds")
	normalizedExcludes := make([]string, len(excludeCwds))
	for i, p := range excludeCwds {
		normalizedExcludes[i] = normalizeDir(p)
	}

	matches, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	if err != nil {
		return fmt.Errorf("globbing transcripts under %s: %w", root, err)
	}
	sort.Strings(matches)

	for _, path := range matches {
		stat, err := os.Stat(path)
		if err != nil {
			continue // file may have been removed between glob and stat
		}

		position := fmt.Sprintf("%d:%d", stat.ModTime().UnixNano(), stat.Size())
		cursor, err := cc.Store.GetCursor(Name, path)
		if err != nil {
			return fmt.Errorf("getting cursor for %s: %w", path, err)
		}
		if cursor == position {
			continue
		}

		mtime := stat.ModTime().UTC()
		if mtime.Before(horizon) {
			if err := cc.Store.SetCursor(Name, path, position); err != nil {
				return fmt.Errorf("setting cursor for %s: %w", path, err)
			}
			continue
		}

		evts, err := sessionEvents(path, mtime, normalizedExcludes, minSegment)
		if err != nil {
			return fmt.Errorf("reading session %s: %w", path, err)
		}

		if err := cc.Store.SetCursor(Name, path, position); err != nil {
			return fmt.Errorf("setting cursor for %s: %w", path, err)
		}

		for _, evt := range evts {
			visit(evt)
		}
	}

	return nil
}

// segmentSummary renders the one-line human-readable summary for one branch run.
//
// The opening line is the segment's OWN first message, not the session's. That
// distinction is the point: a 71-day session's every event previously carried its May
// opening ask, so three unrelated bodies of work were all described as "putting together
// an architectural vision document" — the summary clustering and the review queue both
// read.
func segmentSummary(project string, seg segment) string {
	opening := strings.TrimSpace(strings.ReplaceAll(seg.userTexts[0], "\n", " "))
	if len(opening) > 160 {
		opening = opening[:157] + "..."
	}

	branchNote := ""
	if seg.gitBranch != "" {
		branchNote = " on branch " + seg.gitBranch
	}

	return fmt.Sprintf(
		`Claude Code session in %s%s: %d user messages. Opened with: "%s"`,
		project, branchNote, len(seg.userTexts), opening,
	)
}

// sessionEvents turns one transcript into one event per contiguous branch run.
//
// Finding F15: this used to return a single event dated to the session's LAST message.
// Session e951ef78 ran 71 days across three branches; the run that produced the merge
// PAAS-3905 retro-credits ended 2026-07-09, and the one event unjira emitted was dated
// 2026-07-16 carrying the wrong branch. Every date comparison downstream — the
// reconciler's delta, NarrativesOverlapping, any --since window — was against "when did
// you last type".
//
// Each segment is dated to its OWN last message and carries its OWN branch, which is
// what recovers ProvenanceBranch for the runs that were previously discarded. Every
// segment also carries the full branch set (see segment.allBranches): slicing only helps
// sessions that change branch, and 42 of 79 multi-day sessions never do.
func sessionEvents(
	path string, mtime time.Time, excludeCwds []string, minSegment int,
) ([]events.Event, error) {
	sessionID := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))

	lines, err := jsonlLines(path)
	if err != nil {
		return nil, err
	}

	segs := segments(lines, minSegment)
	if len(segs) == 0 {
		return nil, nil
	}

	stat, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	out := make([]events.Event, 0, len(segs))

	for i, seg := range segs {
		if len(seg.userTexts) == 0 {
			continue
		}
		if seg.cwd != "" && isExcluded(seg.cwd, excludeCwds) {
			continue // unjira's own repo etc. — skip to avoid self-reference loops
		}

		project := filepath.Base(filepath.Dir(path))
		if seg.cwd != "" {
			project = filepath.Base(seg.cwd)
		}

		occurredAt := mtime
		if parsed, err := time.Parse(time.RFC3339, strings.Replace(seg.lastTS, "Z", "+00:00", 1)); err == nil {
			occurredAt = parsed
		}

		// The segment index is part of the ExternalID, not just the file size. Size
		// alone made a growing session re-emit whole-session snapshots (one live
		// session produced 19); size plus index keeps each segment distinct within a
		// snapshot while still deduping an unchanged re-read at insert. The index
		// rather than the branch name because a branch can legitimately appear twice
		// when runs are far enough apart not to coalesce.
		evt := events.NewEvent(
			Name,
			fmt.Sprintf("%s:%d:%d", sessionID, stat.Size(), i),
			occurredAt,
			segmentSummary(project, seg),
		)
		evt.Artifacts["session_id"] = sessionID
		evt.Artifacts["cwd"] = seg.cwd
		evt.Artifacts[events.ArtifactGitBranch] = seg.gitBranch
		events.SetTicketKeys(&evt, seg.orderedKeys)
		// Kept as its own artifact, not appended to the prose keys: a key committed
		// under is stronger evidence than one mentioned, and the correlator ranks on
		// exactly that difference (finding F20).
		events.SetSCMKeys(&evt, seg.scmKeys)
		evt.Artifacts["user_message_count"] = len(seg.userTexts)
		evt.Artifacts["started_at"] = seg.firstTS
		// The interval, not just its end. A segment spanning three weeks and one
		// spanning an hour were previously indistinguishable, both reduced to a single
		// timestamp — see F15's third consequence.
		evt.Artifacts["ended_at"] = seg.lastTS
		evt.Artifacts["session_branches"] = seg.allBranches
		evt.RawRef = path

		out = append(out, evt)
	}

	return out, nil
}

func jsonlLines(path string) ([]map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var lines []map[string]any
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}

		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			continue // malformed line: skip, matching the Python collector's errors="replace" leniency
		}
		lines = append(lines, line)
	}

	return lines, scanner.Err()
}

// messageText extracts human-authored or model-authored text from a
// transcript line. Tool results and command wrappers (content starting
// with '<') are skipped — they are plumbing, not narrative.
func messageText(line map[string]any) string {
	lineType, _ := line["type"].(string)
	if lineType != "user" && lineType != "assistant" {
		return ""
	}

	message, _ := line["message"].(map[string]any)
	if message == nil {
		return ""
	}

	var parts []string
	switch content := message["content"].(type) {
	case string:
		if content != "" {
			parts = append(parts, content)
		}
	case []any:
		for _, block := range content {
			m, ok := block.(map[string]any)
			if !ok || m["type"] != "text" {
				continue
			}
			if text, _ := m["text"].(string); text != "" {
				parts = append(parts, text)
			}
		}
	}

	text := strings.TrimSpace(strings.Join(parts, "\n"))
	if strings.HasPrefix(text, "<") {
		return ""
	}

	return text
}

// normalizeDir returns an absolute, cleaned directory string without a
// trailing separator.
func normalizeDir(path string) string {
	expanded := path
	if rest, ok := strings.CutPrefix(path, "~"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			expanded = filepath.Join(home, rest)
		}
	}

	abs, err := filepath.Abs(expanded)
	if err != nil {
		return filepath.Clean(expanded)
	}

	return filepath.Clean(abs)
}

// isExcluded reports whether cwd equals or is nested under any excluded
// prefix (path-boundary aware).
//
// Uses filepath.Rel rather than strings.HasPrefix so that a sibling sharing
// a string prefix (e.g. "/w/unjira-docs" vs excluded "/w/unjira") is NOT
// excluded.
func isExcluded(cwd string, excludeCwds []string) bool {
	target := normalizeDir(cwd)

	for _, prefix := range excludeCwds {
		if target == prefix {
			return true
		}

		rel, err := filepath.Rel(prefix, target)
		if err != nil {
			continue // different volumes / not comparable — not excluded
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}

	return false
}

func stringOption(options map[string]any, key, fallback string) string {
	if v, ok := options[key].(string); ok && v != "" {
		return v
	}

	return fallback
}

func intOption(options map[string]any, key string, fallback int) int {
	switch v := options[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}

	return fallback
}

func stringSliceOption(options map[string]any, key string) []string {
	raw, ok := options[key]
	if !ok {
		return nil
	}

	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
