// Package rules loads unjira's human-curated markdown rule files (see
// rules/README.md for the intended workflow: corrections distilled from the
// review queue into diffable, git-remote-shareable prose) and makes them
// available to LLM prompts.
//
// Frontmatter is parsed with gopkg.in/yaml.v3, not hand-rolled bufio/strings
// splitting. That dependency costs nothing: testify already pulls
// gopkg.in/yaml.v3 into the module graph, so promoting it from an indirect
// to a direct require in go.mod adds zero new modules and zero new go.sum
// hashes — the earlier reasoning for hand-parsing conflated "not a direct
// dependency" with "not in the module graph," which is not the same thing.
// Hand-splitting each line on the first ':' is also not merely redundant
// with a library, it is wrong on valid YAML: it silently retains the quotes
// on `scope: "correlator"` or `scope: 'correlator'`, and folds a trailing
// `# comment` into the value on `scope: correlator  # comment`. Because a
// bad scope/confidence value fails the whole file's parse (see parseScope,
// parseConfidence) and Load returns on the first error, one such file would
// take every other rule out of every prompt — the exact failure mode this
// package's error-loudly design is meant to avoid, just triggered by the
// hand parser mangling input that was never actually malformed.
//
// This package only reads rules/. Nothing here writes: rule *proposal*
// generation from review-queue corrections (rules.Distill) is a later
// phase-1 slice, not part of this package's job.
package rules

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Scope names the component a rule applies to. A rule's scope determines
// which prompt(s) it can be injected into — see ForScope.
type Scope string

// The three scopes a rule may declare. Reconciler and Estimator are parsed
// and preserved by Load so a rule file targeting either is not rejected
// merely because this package's caller does not yet wire that scope into a
// prompt (see internal/correlator's use of ForScope(rules, ScopeCorrelator)
// only) — internal/reconciler does not exist yet, and no estimator exists
// at all, but a human curating rules/ should not be blocked from writing a
// rule for either ahead of the code that will consume it.
const (
	ScopeCorrelator Scope = "correlator"
	ScopeReconciler Scope = "reconciler"
	ScopeEstimator  Scope = "estimator"
)

// Confidence marks how much a rule should be weighed. Provisional rules are
// loaded and labelled, never filtered out — the review-queue workflow that
// produces them needs them visible so a human (or the model) can judge them,
// not silently dropped before anyone sees them applied.
type Confidence string

// The two confidence levels a rule may declare.
const (
	ConfidenceHigh        Confidence = "high"
	ConfidenceProvisional Confidence = "provisional"
)

// Rule is one parsed markdown rule file.
type Rule struct {
	// Name is the file's basename without .md — the stable identifier a
	// rendered prompt traces back to a specific file when something in it
	// turns out to be wrong.
	Name       string
	Scope      Scope
	Confidence Confidence
	// Learned is the frontmatter date, kept as the string it was written
	// as rather than parsed into a time.Time. A rule file's value is its
	// body — the norm it encodes — not its date; failing an entire load
	// over one file's malformed date would take every other valid rule out
	// of the prompt with it over a typo nobody would notice quickly (dates
	// are not validated against anything at read time the way scope/
	// confidence are, since nothing here orders rules by Learned). Losing a
	// rule silently would be worse than keeping a possibly-malformed date
	// string that is still human-legible in Render's output.
	Learned string
	Source  string
	// Body is the markdown after the closing frontmatter delimiter, trimmed
	// of leading/trailing whitespace. Multi-paragraph prose is preserved
	// as-is.
	Body string
}

// frontmatterDelimiter marks the start and end of a rule file's frontmatter
// block.
const frontmatterDelimiter = "---"

// Load reads every *.md file in dir and parses it as a Rule, skipping any
// file with no frontmatter (rules/README.md, which documents the format but
// is not itself a rule) rather than treating "no frontmatter" as an error —
// the format doc must be able to live alongside the rules it describes.
//
// A missing dir is a no-op returning an empty set, not an error: a fresh
// clone or a deployment that has not seeded any rules yet must still run.
// Any other error reading the directory, or any malformed rule file found
// within it, fails loudly and names the offending file — a rule that
// silently never reaches a prompt is worse than a startup failure, since the
// operator would otherwise believe a norm is in effect when it is not.
func Load(dir string) ([]Rule, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rules: reading directory %s: %w", dir, err)
	}

	var out []Rule
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}

		path := filepath.Join(dir, entry.Name())

		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("rules: reading %s: %w", path, err)
		}

		rule, ok, err := parseRule(entry.Name(), string(body))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		out = append(out, rule)
	}

	return out, nil
}

// parseRule parses one rule file's contents. filename is used only for
// error messages and to derive Rule.Name. ok is false, with a nil error, for
// a file that has no frontmatter at all (e.g. rules/README.md) — the one
// case Load treats as a deliberate skip rather than a malformed rule.
func parseRule(filename, contents string) (rule Rule, ok bool, err error) {
	name := strings.TrimSuffix(filename, filepath.Ext(filename))

	lines := strings.Split(contents, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != frontmatterDelimiter {
		return Rule{}, false, nil
	}

	closing := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == frontmatterDelimiter {
			closing = i
			break
		}
	}
	if closing == -1 {
		return Rule{}, false, fmt.Errorf("rules: %s: frontmatter has no closing %q delimiter", filename, frontmatterDelimiter)
	}

	fields, err := parseFrontmatterFields(filename, lines[1:closing])
	if err != nil {
		return Rule{}, false, err
	}

	scope, err := parseScope(filename, fields.Scope)
	if err != nil {
		return Rule{}, false, err
	}

	confidence, err := parseConfidence(filename, fields.Confidence)
	if err != nil {
		return Rule{}, false, err
	}

	body := strings.TrimSpace(strings.Join(lines[closing+1:], "\n"))

	return Rule{
		Name:       name,
		Scope:      scope,
		Confidence: confidence,
		Learned:    fields.Learned,
		Source:     fields.Source,
		Body:       body,
	}, true, nil
}

// frontmatterFields is the YAML shape of a rule file's frontmatter block.
// Learned is deliberately a string, not a time.Time — see Rule.Learned's
// doc comment for why: yaml.v3 would otherwise parse an unquoted
// `learned: 2026-07-16` into a time.Time, and this package wants that
// scalar preserved exactly as written. Unknown keys are accepted and
// ignored (plain yaml.Unmarshal into a struct already does this; do not add
// yaml.KnownFields(true)), so a future field can be added to
// rules/README.md's format without every existing file needing a code
// change first; the four keys this package actually reads are validated
// individually by their own callers (parseScope, parseConfidence).
type frontmatterFields struct {
	Scope      string `yaml:"scope"`
	Confidence string `yaml:"confidence"`
	Learned    string `yaml:"learned"`
	Source     string `yaml:"source"`
}

// parseFrontmatterFields parses the YAML block between a rule file's ---
// delimiters into a frontmatterFields. A malformed YAML block (e.g.
// mismatched quotes, bad indentation) is a loud error naming the file.
func parseFrontmatterFields(filename string, lines []string) (frontmatterFields, error) {
	var fields frontmatterFields

	if err := yaml.Unmarshal([]byte(strings.Join(lines, "\n")), &fields); err != nil {
		return frontmatterFields{}, fmt.Errorf("rules: %s: parsing frontmatter: %w", filename, err)
	}

	return fields, nil
}

// parseScope validates raw against the closed set of known scopes. Unknown
// is a loud error naming the file and the bad value — matching how
// unjira's other closed enums (e.g. correlator's Role) reject an unrecognized
// value rather than silently ignoring it, since a rule that never reaches a
// prompt because its scope was mistyped is worse than a startup failure.
func parseScope(filename, raw string) (Scope, error) {
	switch Scope(raw) {
	case ScopeCorrelator, ScopeReconciler, ScopeEstimator:
		return Scope(raw), nil
	default:
		return "", fmt.Errorf("rules: %s: unknown scope %q: must be one of correlator, reconciler, estimator", filename, raw)
	}
}

// parseConfidence validates raw against the closed set of known confidence
// levels, with the same loud-error rationale as parseScope.
func parseConfidence(filename, raw string) (Confidence, error) {
	switch Confidence(raw) {
	case ConfidenceHigh, ConfidenceProvisional:
		return Confidence(raw), nil
	default:
		return "", fmt.Errorf("rules: %s: unknown confidence %q: must be one of high, provisional", filename, raw)
	}
}

// ForScope returns the subset of all whose Scope matches scope, in their
// original order. Does not mutate all: it builds a fresh slice rather than
// filtering in place, so a caller that loaded once and calls ForScope for
// multiple scopes (or holds onto the original slice) never sees it change
// out from under it.
func ForScope(all []Rule, scope Scope) []Rule {
	var out []Rule
	for _, r := range all {
		if r.Scope == scope {
			out = append(out, r)
		}
	}

	return out
}

// Render formats rules for inclusion in a system prompt. Returns "" for an
// empty rules, so a caller can skip the whole section (header included)
// rather than emitting a header over nothing.
//
// Each rule's Name is included so a wrong rule is traceable back to its
// file, and a provisional rule is marked distinctly from a high-confidence
// one so the model can weigh it accordingly (see rules.Confidence's doc
// comment: provisional rules must stay visible, not be dropped). Source and
// Learned are omitted — a system prompt is not the place to spend tokens on
// provenance a human, not the model, would use to audit or prune a rule;
// nothing about applying a rule during clustering/matching depends on when
// or from what correction it was learned.
func Render(rules []Rule) string {
	if len(rules) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("Team-specific rules learned from prior corrections:\n")
	for _, r := range rules {
		label := "high confidence"
		if r.Confidence == ConfidenceProvisional {
			label = "provisional — weigh accordingly"
		}
		fmt.Fprintf(&b, "- [%s, %s] %s\n", r.Name, label, r.Body)
	}

	return b.String()
}
