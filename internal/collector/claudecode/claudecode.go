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

		event, err := sessionEvent(path, mtime, normalizedExcludes)
		if err != nil {
			return fmt.Errorf("reading session %s: %w", path, err)
		}

		if err := cc.Store.SetCursor(Name, path, position); err != nil {
			return fmt.Errorf("setting cursor for %s: %w", path, err)
		}

		if event != nil {
			visit(*event)
		}
	}

	return nil
}

// sessionMeta is what scanLines accumulates from a transcript's JSONL lines.
type sessionMeta struct {
	cwd, gitBranch, firstTS, lastTS string
	userTexts                       []string
	orderedKeys                     []string
}

// scanLines walks a transcript's lines once, tracking cwd/branch/timestamps,
// every candidate ticket key (from message text and, later, the branch
// name), and every user-authored message.
func scanLines(lines []map[string]any) sessionMeta {
	var meta sessionMeta

	keys := make(map[string]bool)
	addKey := func(key string) {
		if !keys[key] {
			keys[key] = true
			meta.orderedKeys = append(meta.orderedKeys, key)
		}
	}

	for _, line := range lines {
		if v, _ := line["cwd"].(string); v != "" {
			meta.cwd = v
		}
		if v, _ := line["gitBranch"].(string); v != "" {
			meta.gitBranch = v
		}
		if ts, _ := line["timestamp"].(string); ts != "" {
			if meta.firstTS == "" {
				meta.firstTS = ts
			}
			meta.lastTS = ts
		}

		text := messageText(line)
		if text == "" {
			continue
		}

		for _, key := range events.ExtractTicketKeys(text) {
			addKey(key)
		}
		if line["type"] == "user" {
			meta.userTexts = append(meta.userTexts, text)
		}
	}

	if meta.gitBranch != "" {
		for _, key := range events.ExtractTicketKeys(meta.gitBranch) {
			addKey(key)
		}
	}

	return meta
}

// sessionSummary renders the one-line human-readable summary for a session.
func sessionSummary(project string, meta sessionMeta) string {
	opening := strings.TrimSpace(strings.ReplaceAll(meta.userTexts[0], "\n", " "))
	if len(opening) > 160 {
		opening = opening[:157] + "..."
	}

	branchNote := ""
	if meta.gitBranch != "" {
		branchNote = " on branch " + meta.gitBranch
	}

	return fmt.Sprintf(
		`Claude Code session in %s%s: %d user messages. Opened with: "%s"`,
		project, branchNote, len(meta.userTexts), opening,
	)
}

func sessionEvent(path string, mtime time.Time, excludeCwds []string) (*events.Event, error) {
	sessionID := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))

	lines, err := jsonlLines(path)
	if err != nil {
		return nil, err
	}

	meta := scanLines(lines)

	if len(meta.userTexts) == 0 {
		return nil, nil
	}

	if meta.cwd != "" && isExcluded(meta.cwd, excludeCwds) {
		return nil, nil // unjira's own repo etc. — skip to avoid self-reference loops
	}

	project := filepath.Base(filepath.Dir(path))
	if meta.cwd != "" {
		project = filepath.Base(meta.cwd)
	}

	stat, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	occurredAt := mtime
	if parsed, err := time.Parse(time.RFC3339, strings.Replace(meta.lastTS, "Z", "+00:00", 1)); err == nil {
		occurredAt = parsed
	}

	event := events.NewEvent(
		Name,
		fmt.Sprintf("%s:%d", sessionID, stat.Size()),
		occurredAt,
		sessionSummary(project, meta),
	)
	event.Artifacts["session_id"] = sessionID
	event.Artifacts["cwd"] = meta.cwd
	event.Artifacts["git_branch"] = meta.gitBranch
	event.Artifacts["ticket_keys"] = toAnySlice(meta.orderedKeys)
	event.Artifacts["user_message_count"] = len(meta.userTexts)
	event.Artifacts["started_at"] = meta.firstTS
	event.RawRef = path

	return &event, nil
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

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}

	return out
}
