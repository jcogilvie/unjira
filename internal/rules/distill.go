package rules

// distill.go is phase 1's slice 7: turning a reviewer's free-text corrections into
// candidate rule files.
//
// THE LOOP THIS CLOSES. triage persists actions.feedback on reject and edit, and until
// now nothing read it. A reviewer could say "progress comments that only restate
// 'committed and ran tests' are not worth posting" and the reconciler would propose the
// same shape again next pass, because the correction lived in a column no prompt
// consulted. Rule READING has worked since the rules package landed (Load, ForScope,
// Render, injected into both the correlator's and reconciler's prompts); rule WRITING is
// what was missing.
//
// WRITES NOTHING. Distill returns candidates and stops. The phase-1 spec is explicit —
// "No file I/O in this package — writing rules/*.md happens in triage, only on
// keep/modify" — and the reason is that a rule file changes agent behaviour for every
// future pass on every narrative. That is a decision a human makes while looking at the
// drafted text, not something a distillation pass does on its own authority. It is the
// same argument gate.Applier embodies for tracker writes.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/llm"
)

// Correction is one reviewer ruling with the text they wrote, as Distill needs it.
//
// Carries the drafted BODY as well as the feedback, because a correction is only legible
// against what it corrected: "name the affected resource types" is a norm about
// specificity when read next to a comment that said "the audit", and unintelligible
// alone. ActionType is here for the same reason — a norm about comments is not a norm
// about transitions, and a distilled rule that conflates them would be applied to both.
type Correction struct {
	ActionID   int64
	ActionType string
	IssueKey   string
	// Body is the prose the reviewer was reacting to.
	Body string
	// Feedback is what the reviewer wrote. The reason this package exists.
	Feedback string
}

// Candidate is a drafted rule, not yet written anywhere.
//
// Deliberately a distinct type from Rule rather than reusing it. A Rule has been read
// from a file a human accepted; a Candidate is a proposal. Collapsing them would make
// "has anyone agreed to this?" unanswerable from the type, which is exactly the
// distinction between proposed and applied that the actions queue keeps.
type Candidate struct {
	Name       string
	Scope      Scope
	Confidence Confidence
	Body       string
	// SourceActionIDs are the corrections this candidate came from, so a reviewer can
	// see the evidence and a future reader can trace a rule back to the rulings that
	// produced it.
	SourceActionIDs []int64
}

// FileContents renders the candidate as the file triage would write.
//
// Here rather than in triage because this package owns the frontmatter format — parseRule
// reads it — and a writer living elsewhere would be a second, drifting definition of the
// same format. triage still decides WHETHER to write; this only says what the bytes are.
//
// The learned date is stamped at render time. Rule.Learned deliberately keeps its value
// as an unparsed string, so this only has to produce something human-legible.
func (c Candidate) FileContents() string {
	var b strings.Builder

	b.WriteString("---\n")
	fmt.Fprintf(&b, "scope: %s\n", c.Scope)
	fmt.Fprintf(&b, "confidence: %s\n", c.Confidence)
	fmt.Fprintf(&b, "learned: %s\n", time.Now().UTC().Format("2006-01-02"))
	if len(c.SourceActionIDs) > 0 {
		fmt.Fprintf(&b, "source: triage feedback on action(s) %s\n", joinIDs(c.SourceActionIDs))
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(c.Body))
	b.WriteString("\n")

	return b.String()
}

// joinIDs formats action IDs for the source line.
func joinIDs(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%d", id))
	}

	return strings.Join(parts, ", ")
}

// ruleName is what a candidate's filename may contain: lowercase, digits, dashes.
//
// Strict because the name becomes a path. `../escape` would write outside the rules
// directory, and a name with spaces produces a file that Render can display but nobody
// can reference in a commit or a conversation. Rejecting is safe — the model can be asked
// again — where sanitising silently would produce a name the reviewer did not approve.
var ruleName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Distill drafts candidate rules from corrections. It makes at most one model call and
// writes nothing.
//
// No corrections means no call: a learn-check over a queue nobody corrected must not
// spend a request to conclude there is nothing to learn.
//
// An empty array back is a valid answer, not a failure. "These corrections do not
// generalise" is frequently correct — a typo fix is not a norm — and treating it as an
// error would pressure the model into inventing rules from one-off edits, which is the
// failure mode that would poison every future prompt.
func Distill(
	ctx context.Context, client llm.Client, corrections []Correction,
) ([]Candidate, llm.Usage, error) {
	if len(corrections) == 0 {
		return nil, llm.Usage{}, nil
	}

	raw, usage, err := client.Complete(ctx, distillSystemPrompt, buildDistillPrompt(corrections))
	if err != nil {
		return nil, usage, fmt.Errorf("distilling %d correction(s): %w", len(corrections), err)
	}

	candidates, err := parseCandidates(raw, corrections)
	if err != nil {
		return nil, usage, err
	}

	return candidates, usage, nil
}

// rawCandidate is the wire shape, before validation.
type rawCandidate struct {
	Name       string  `json:"name"`
	Scope      string  `json:"scope"`
	Confidence string  `json:"confidence"`
	Body       string  `json:"body"`
	SourceIDs  []int64 `json:"source_action_ids"`
}

// parseCandidates validates the model's reply.
//
// Every check here exists because Load fails a WHOLE FILE on bad frontmatter and returns
// on the first error — so one unloadable rule takes every other rule out of both prompts
// with it. Validating at draft time means a reviewer never gets offered a candidate that
// would break rule loading if they accepted it.
func parseCandidates(raw string, corrections []Correction) ([]Candidate, error) {
	trimmed := strings.TrimSpace(stripCodeFence(raw))

	var decoded []rawCandidate
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return nil, fmt.Errorf("parsing distilled rules: %w (response began %q)",
			err, firstRunes(trimmed, 80))
	}

	out := make([]Candidate, 0, len(decoded))
	for i, d := range decoded {
		candidate, err := validateCandidate(d, corrections)
		if err != nil {
			return nil, fmt.Errorf("distilled rule %d: %w", i, err)
		}
		out = append(out, candidate)
	}

	return out, nil
}

// validateCandidate turns one wire candidate into a Candidate, or explains why not.
func validateCandidate(d rawCandidate, corrections []Correction) (Candidate, error) {
	if !ruleName.MatchString(d.Name) {
		return Candidate{}, fmt.Errorf(
			"name %q is not usable as a filename: expected lowercase words joined by dashes "+
				"(the name becomes a path, so slashes and spaces are refused rather than sanitised)",
			d.Name)
	}

	scope, err := parseScope("distilled rule "+d.Name, d.Scope)
	if err != nil {
		return Candidate{}, err
	}

	// An omitted or unrecognised confidence becomes provisional rather than erroring.
	// One correction is one data point, and ConfidenceHigh is reserved for norms that
	// have held up — so the safe direction is less weight, not a failed pass.
	confidence := ConfidenceProvisional
	if strings.TrimSpace(d.Confidence) != "" {
		parsed, perr := parseConfidence("distilled rule "+d.Name, d.Confidence)
		if perr == nil {
			confidence = parsed
		}
	}

	if strings.TrimSpace(d.Body) == "" {
		return Candidate{}, fmt.Errorf(
			"body is empty: a rule with a name and no body renders as a heading with nothing " +
				"under it, which reads as guidance while carrying none")
	}

	ids := d.SourceIDs
	if len(ids) == 0 {
		// Attribute to every correction shown rather than none. A rule with no
		// provenance cannot be traced back to the ruling that motivated it, and the
		// model omitting the field should not cost that.
		for _, c := range corrections {
			ids = append(ids, c.ActionID)
		}
	}

	return Candidate{
		Name: d.Name, Scope: scope, Confidence: confidence,
		Body: strings.TrimSpace(d.Body), SourceActionIDs: ids,
	}, nil
}

// stripCodeFence removes a ```json fence, which models add despite being asked not to.
func stripCodeFence(s string) string {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "```") {
		return trimmed
	}

	if i := strings.Index(trimmed, "\n"); i >= 0 {
		trimmed = trimmed[i+1:]
	}

	return strings.TrimSuffix(strings.TrimSpace(trimmed), "```")
}

// firstRunes is a rune-safe prefix for error messages, so a multi-byte reply is not cut
// mid-character into mojibake.
func firstRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}

	return string(runes[:n]) + "…"
}

// buildDistillPrompt renders the corrections.
//
// Every correction in one prompt, deliberately: the spec allows a candidate per
// correction OR per cluster of related corrections, and a model cannot cluster what it
// is not shown. The drafted body accompanies each one because a correction is only
// legible against what it corrected.
func buildDistillPrompt(corrections []Correction) string {
	var b strings.Builder

	b.WriteString("Reviewer corrections:\n\n")
	for i, c := range corrections {
		fmt.Fprintf(&b, "%d. action %d — %s on %s\n", i+1, c.ActionID, c.ActionType, c.IssueKey)
		fmt.Fprintf(&b, "   unjira drafted: %q\n", c.Body)
		fmt.Fprintf(&b, "   reviewer said:  %q\n\n", c.Feedback)
	}

	return b.String()
}

// distillSystemPrompt asks for generalisable norms and refuses fabrication.
//
// The no-invention clause is not boilerplate. Every other unjira prompt carries one
// because a hallucinated CLAIM reaches a review queue, where a human catches it — but a
// hallucinated RULE is injected into every future prompt and shapes proposals nobody
// traces back to it. This is the one output whose errors compound.
//
// It is also told to prefer returning nothing. A distiller that always finds a rule
// converts one-off corrections into permanent norms, and the accumulated result is a
// prompt full of guidance nobody agreed to.
const distillSystemPrompt = `You turn a reviewer's corrections of drafted Jira updates into reusable norms.

Return ONLY a JSON array, no prose and no markdown fences:
[{"name":"kebab-case-name","scope":"correlator"|"reconciler"|"estimator","confidence":"high"|"provisional","body":"the norm, in prose","source_action_ids":[1,2]}]

Rules for what you return:
- DO NOT INVENT norms the corrections do not support. A rule you draft is injected into every future prompt, so an unfounded one changes behaviour permanently and invisibly.
- Prefer returning [] over a weak rule. A typo fix, a one-off wording preference, or a correction specific to one ticket is NOT a norm. Most single corrections should yield nothing.
- Draft one rule per distinct norm. Several corrections making the same point are ONE rule; one correction making several points may be several.
- Use "provisional" unless the same norm appears in multiple independent corrections. One correction is one data point.
- Scope by which component would need to change: correlator for clustering/attribution, reconciler for what gets drafted and whether to act, estimator for sizing.
- The body states the norm and, briefly, why. Write it for the model that will read it in a prompt, not for a changelog.
- The name must be lowercase words joined by dashes: it becomes a filename.`
