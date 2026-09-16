package claudecode

// facts.go extracts what a session DID, as a bounded set of deterministic phrases —
// finding F25.
//
// The summary used to be the first user message plus a count, so it stated `3 user
// messages` and showed one: it advertised evidence it withheld. A reconciler rationale
// of the form "no PR or completion evidence yet" was therefore unfalsifiable rather
// than wrong — the same sentence would be produced for a session that shipped and
// closed the ticket. Measured: 388 of 419 events (93%) omitted content, worst case 376
// of 377 messages.
//
// DELIBERATELY NOT "include every message." `userTexts` for a 377-message session is
// exactly the volume F16 attributes 93.7% of clustering's prompt cost to, so inlining
// it would undo that saving at the source. These facts are a FIXED-size addition: a
// session that commits forty times contributes the same handful of words as one that
// commits once.
//
// DETERMINISTIC, so this stays a collector's job. CLAUDE.md's invariant is that
// extraction is a pure function's work and judgment is the model's; "did this session
// commit?" is extraction. The README's onboarding-backfill entry proposes a cheap model
// for richer distillation, which is a different and larger thing.

import (
	"regexp"
	"slices"
)

// factRule maps a command shape to the phrase it licenses.
//
// Reading commands are deliberately absent, mirroring scmKeys: `git log` is not
// evidence of a commit and `gh pr view` is not evidence of having opened one. F20's
// measurement found 8 of 15 keys appeared ONLY in reading commands, and the same
// asymmetry applies to facts — claiming a session committed because it ran `git log`
// would manufacture exactly the false confidence F25 is about.
//
// Ordered, and that order is the render order: a declared sequence rather than
// observation order, so two sessions doing the same things read alike and one
// session's summary does not churn between passes.
var factRules = []struct {
	phrase  string
	pattern *regexp.Regexp
}{
	{"committed", regexp.MustCompile(`git commit`)},
	{"created a branch", regexp.MustCompile(`git checkout -b|git switch -c`)},
	{"opened a PR", regexp.MustCompile(`gh pr create`)},
	{"updated a PR", regexp.MustCompile(`gh pr edit`)},
	{"ran tests", regexp.MustCompile(`helm unittest|go test|\./test\.sh|pytest|make e2e`)},
}

// sessionFacts returns the phrases a segment's tool calls license, deduplicated and in
// factRules order.
//
// Returns nil when nothing matched, so a session that only talked keeps exactly the
// summary it had before — an empty facts clause would read as "did nothing", which is a
// different claim from "we have no record of doing anything".
func sessionFacts(lines []map[string]any) []string {
	var seen []string

	for _, line := range lines {
		for _, cmd := range toolCommands(line) {
			for _, rule := range factRules {
				if rule.pattern.MatchString(cmd) && !slices.Contains(seen, rule.phrase) {
					seen = append(seen, rule.phrase)
				}
			}
		}
	}

	if len(seen) == 0 {
		return nil
	}

	// Sort into factRules order rather than first-seen order.
	slices.SortStableFunc(seen, func(a, b string) int {
		return factRuleIndex(a) - factRuleIndex(b)
	})

	return seen
}

// factRuleIndex is a phrase's position in factRules, for the stable ordering.
func factRuleIndex(phrase string) int {
	for i, rule := range factRules {
		if rule.phrase == phrase {
			return i
		}
	}

	return len(factRules)
}

// toolCommands returns every shell command in a transcript line's tool calls.
//
// Only `command` inputs, not the whole marshalled input: a fact is about something the
// session RAN, and matching against a tool's arbitrary JSON would let a file path or a
// PR body containing "git commit" license the phrase. scmKeys deliberately searches the
// whole input for MCP calls because a KEY can legitimately live in any field; a fact
// cannot.
func toolCommands(line map[string]any) []string {
	message, _ := line["message"].(map[string]any)
	if message == nil {
		return nil
	}

	blocks, _ := message["content"].([]any)

	var out []string
	for _, block := range blocks {
		b, ok := block.(map[string]any)
		if !ok || b["type"] != "tool_use" {
			continue
		}

		input, ok := b["input"].(map[string]any)
		if !ok {
			continue
		}

		if cmd, ok := input["command"].(string); ok && cmd != "" {
			out = append(out, cmd)
		}
	}

	return out
}
