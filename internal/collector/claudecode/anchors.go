package claudecode

// anchors.go emits one event per tool call that creates a pull request.
//
// A `gh pr create` call is a structured record of a discrete act — the same fact
// GitHub's `:opened` event records, seen from the other side. Inside a segment it was
// only a phrase ("Did: opened a PR"), and a segment can join one narrative, so in
// subagent-first work, where one transcript opens several PRs, most PRs' narratives
// had no transcript evidence at all. An anchor per call gives each PR its own.
//
// The identity comes from the call's RESULT, not its arguments: `gh pr create` prints
// the created PR's URL and the GitHub MCP returns it in JSON, which yields
// `<owner>/<repo>#<N>` exactly — the identifier the github collector keys on. Only the
// result paired with a recognized PR-creating call is read; tool results in general
// stay plumbing (see messageText).
//
// Anchors are work evidence — opening a PR is a thing somebody did — and deliberately
// feed no provenance tier: no ArtifactGitBranch, no keys. They add a clusterable event
// whose summary names the PR; whether that shared identifier is enough, or a
// deterministic join on events.ArtifactPullRequest is needed, is a later measurement.

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/events"
)

// mcpCreatePullRequest is the GitHub MCP's PR-creating tool, the only such name in
// any measured transcript (129 calls).
const mcpCreatePullRequest = "mcp__github__create_pull_request"

// toolGHPRCreate labels a shell call recognized by ghPRCreateInvocations.
const toolGHPRCreate = "gh pr create"

// Collector-private anchor artifacts. Nothing outside this package reads them; the
// cross-collector one is events.ArtifactPullRequest.
const (
	// artifactAnchorKind marks an event as an anchor rather than a segment.
	artifactAnchorKind = "anchor_kind"
	anchorPRCreate     = "pr_create"

	artifactToolUseID = "tool_use_id"
	artifactTool      = "tool"
	// artifactPROutcome is what the result says happened: created, failed,
	// unconfirmed, or ambiguous (see the outcome* constants).
	artifactPROutcome = "pr_create_outcome"
	// artifactPRResolution is where the PR's identity came from: the tool result
	// (exact owner/repo#N), the call's literal arguments (repo/head, never a
	// number), or nowhere.
	artifactPRResolution = "pr_resolution"
	// artifactPRReason says why the identity is not exact. Set whenever
	// artifactPRResolution is not "tool_result".
	artifactPRReason     = "pr_unresolved_reason"
	artifactPRURL        = "pr_url"
	artifactPRRepo       = "pr_repo"
	artifactPRHeadBranch = "pr_head_branch"
	// artifactPRCandidates lists every PR an ambiguous result named.
	artifactPRCandidates = "pr_candidates"
	// artifactAwaitingResult, on segment events, lists the PR-creating calls whose
	// result the transcript did not yet hold, so their deferral is visible.
	artifactAwaitingResult = "pr_creates_awaiting_result"
)

const (
	outcomeCreated     = "created"
	outcomeFailed      = "failed"
	outcomeUnconfirmed = "unconfirmed"
	outcomeAmbiguous   = "ambiguous"

	resolutionToolResult = "tool_result"
	resolutionArguments  = "arguments"
	resolutionUnresolved = "unresolved"

	// summaryCandidateLimit bounds how many ambiguous candidates a summary lists;
	// the artifact always holds all of them.
	summaryCandidateLimit = 10
	excerptLimit          = 200
)

// pullRequestURL matches a PR's web URL on any host, so a GHES instance resolves too.
// Requiring digits excludes push output's `/pull/new/<branch>` suggestion.
var pullRequestURL = regexp.MustCompile(`https?://[^/\s"'<>]+/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/(\d+)`)

// ghAlreadyExists is gh's message when the branch already has a PR. The output carries
// that PR's URL, so without this check a failure hidden by `| tail` would resolve as a
// creation of a PR this call did not create.
var ghAlreadyExists = regexp.MustCompile(`a pull request for branch .* already exists`)

// prCreateCall is one recognized PR-creating tool call.
type prCreateCall struct {
	toolUseID string
	tool      string
	ts, cwd   string
	args      prArgs
}

// toolResult is the paired output of a tool call.
type toolResult struct {
	text    string
	isError bool
	ts      string
}

// prAnchor is a call resolved against its result.
type prAnchor struct {
	call       prCreateCall
	outcome    string
	resolution string
	reasons    []string
	pr, url    string
	candidates []string
	ts         string
}

// prCreateCalls returns the PR-creating calls among a line's tool_use blocks.
//
// A block without an id cannot be paired with a result or keyed, and Claude Code
// writes none; it is not an anchor.
func prCreateCalls(line map[string]any, cwd string) []prCreateCall {
	message, _ := line["message"].(map[string]any)
	if message == nil {
		return nil
	}

	blocks, _ := message["content"].([]any)
	ts, _ := line["timestamp"].(string)

	var out []prCreateCall
	for _, block := range blocks {
		b, ok := block.(map[string]any)
		if !ok || b["type"] != blockTypeToolUse {
			continue
		}

		id, _ := b["id"].(string)
		name, _ := b["name"].(string)
		input, _ := b["input"].(map[string]any)
		if id == "" || input == nil {
			continue
		}

		call := prCreateCall{toolUseID: id, ts: ts, cwd: cwd}

		if name == mcpCreatePullRequest {
			call.tool = name
			call.args = mcpArgs(input)
			out = append(out, call)

			continue
		}

		command, _ := input["command"].(string)
		invocations := ghPRCreateInvocations(command)
		if len(invocations) == 0 {
			continue
		}

		call.tool = toolGHPRCreate
		if len(invocations) == 1 {
			call.args = parsePRCreateArgs(invocations[0])
		} else {
			call.args.notes = append(call.args.notes, fmt.Sprintf(
				"command invokes gh pr create at %d sites, so no one site's flags describe the call",
				len(invocations)))
		}
		out = append(out, call)
	}

	return out
}

// mcpArgs reads the GitHub MCP tool's structured input. Its fields are literal by
// construction, so only their presence and type are checked.
func mcpArgs(input map[string]any) prArgs {
	var out prArgs

	owner, _ := input["owner"].(string)
	repo, _ := input["repo"].(string)
	if owner != "" && repo != "" {
		out.repo = owner + "/" + repo
	}
	if head, _ := input["head"].(string); head != "" {
		out.head = headBranch(head)
	}
	out.title, _ = input["title"].(string)

	return out
}

// toolResultsFor returns the results paired with the wanted tool_use ids, by id.
func toolResultsFor(lines []map[string]any, want map[string]bool) map[string]toolResult {
	out := make(map[string]toolResult, len(want))
	if len(want) == 0 {
		return out
	}

	for _, line := range lines {
		message, _ := line["message"].(map[string]any)
		if message == nil {
			continue
		}

		blocks, _ := message["content"].([]any)
		ts, _ := line["timestamp"].(string)

		for _, block := range blocks {
			b, ok := block.(map[string]any)
			if !ok || b["type"] != "tool_result" {
				continue
			}

			id, _ := b["tool_use_id"].(string)
			if !want[id] {
				continue
			}

			isError, _ := b["is_error"].(bool)
			out[id] = toolResult{text: resultText(b["content"]), isError: isError, ts: ts}
		}
	}

	return out
}

// resultText flattens a tool_result's content: a string (Bash) or a list of text
// blocks (MCP). Anything else carries no text this can read.
func resultText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			if m, ok := item.(map[string]any); ok {
				if text, _ := m["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}

		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// resolvePRAnchor decides what a call's result says it did.
//
// The order is the trust order: an error means nothing was created whatever the output
// contains; exactly one PR URL is an exact identity; several is a fact about the
// output that cannot be pinned to this call; none falls back to the call's literal
// arguments, which can name a repo and branch but never a number.
func resolvePRAnchor(call prCreateCall, res toolResult) prAnchor {
	a := prAnchor{call: call, ts: res.ts}
	if a.ts == "" {
		a.ts = call.ts
	}

	refs, urls := pullRequestRefs(res.text)

	switch {
	case res.isError:
		a.outcome = outcomeFailed
		a.reasons = append(a.reasons, "tool result is an error: "+excerpt(res.text))
	case ghAlreadyExists.MatchString(res.text):
		a.outcome = outcomeFailed
		a.reasons = append(a.reasons, "gh reported the branch already has a pull request: "+excerpt(res.text))
	case len(refs) == 1:
		a.outcome = outcomeCreated
		a.resolution = resolutionToolResult
		a.pr, a.url = refs[0], urls[0]

		return a
	case len(refs) > 1:
		a.outcome = outcomeAmbiguous
		a.candidates = refs
		a.reasons = append(a.reasons, fmt.Sprintf(
			"tool result names %d distinct pull requests; which this call created is not recorded", len(refs)))
	default:
		a.outcome = outcomeUnconfirmed
		a.reasons = append(a.reasons, "tool result names no pull request URL: "+excerpt(res.text))
	}

	a.resolution = resolutionUnresolved
	if call.args.repo != "" || call.args.head != "" {
		a.resolution = resolutionArguments
	}
	a.reasons = append(a.reasons, call.args.notes...)

	return a
}

// pullRequestRefs returns the distinct `<owner>/<repo>#<N>` a text names, in order of
// first appearance, with the URL each was first seen as.
func pullRequestRefs(text string) (refs, urls []string) {
	for _, m := range pullRequestURL.FindAllStringSubmatch(text, -1) {
		ref := fmt.Sprintf("%s/%s#%s", m[1], m[2], m[3])
		if slices.Contains(refs, ref) {
			continue
		}
		refs = append(refs, ref)
		urls = append(urls, m[0])
	}

	return refs, urls
}

// excerpt is the first non-empty line of a result, bounded, for a reason string.
func excerpt(text string) string {
	for l := range strings.SplitSeq(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > excerptLimit {
				l = l[:excerptLimit-3] + "..."
			}

			return l
		}
	}

	return "(empty)"
}

// anchorEvent renders a resolved anchor as an event.
//
// ExternalID is `pr_create:<tool_use_id>` — no transcript identity, and no size. The
// tool_use id is immutable per call, so a growing transcript re-emits the same id and
// dedupes; and a repeated id IS the same call, which matters because resumed sessions
// copy history into new files (1,841 tool_use ids measured in more than one root
// transcript). The `pr_create:` prefix cannot collide with a segment id, which starts
// with a session UUID or `agent-`.
func anchorEvent(t transcript, a prAnchor, fallback time.Time) events.Event {
	occurredAt := fallback
	if parsed, ok := parseTimestamp(a.ts); ok {
		occurredAt = parsed
	}

	evt := events.NewEvent(
		Name, anchorPRCreate+":"+a.call.toolUseID, occurredAt, anchorSummary(t, a),
	)
	evt.RawRef = t.path

	setTranscriptArtifacts(evt.Artifacts, t)
	evt.Artifacts["cwd"] = a.call.cwd
	evt.Artifacts[artifactAnchorKind] = anchorPRCreate
	evt.Artifacts[artifactToolUseID] = a.call.toolUseID
	evt.Artifacts[artifactTool] = a.call.tool
	evt.Artifacts[artifactPROutcome] = a.outcome
	evt.Artifacts[artifactPRResolution] = a.resolution

	if a.pr != "" {
		evt.Artifacts[events.ArtifactPullRequest] = a.pr
		evt.Artifacts[artifactPRURL] = a.url

		return evt
	}

	evt.Artifacts[artifactPRReason] = strings.Join(a.reasons, "; ")
	setIfNonEmpty(evt.Artifacts, artifactPRRepo, a.call.args.repo)
	setIfNonEmpty(evt.Artifacts, artifactPRHeadBranch, a.call.args.head)
	if len(a.candidates) > 0 {
		candidates := make([]any, len(a.candidates))
		for i, c := range a.candidates {
			candidates[i] = c
		}
		evt.Artifacts[artifactPRCandidates] = candidates
	}

	return evt
}

// anchorSummary states what the call did in the same voice as a segment summary, and
// claims an opened PR only when the result named exactly one.
func anchorSummary(t transcript, a prAnchor) string {
	lead := fmt.Sprintf("%s in %s", sessionLabel(t), projectName(t, a.call.cwd))

	title := ""
	if a.call.args.title != "" {
		title = fmt.Sprintf(`: "%s"`, truncate(a.call.args.title, 160))
	}

	switch a.outcome {
	case outcomeCreated:
		return fmt.Sprintf("%s opened pull request %s%s", lead, a.pr, title)
	case outcomeFailed:
		return fmt.Sprintf("%s tried to open a pull request%s%s and the call failed: %s",
			lead, target(a.call.args), title, a.reasons[0])
	case outcomeAmbiguous:
		named := a.candidates
		more := ""
		if len(named) > summaryCandidateLimit {
			more = fmt.Sprintf(" and %d more", len(named)-summaryCandidateLimit)
			named = named[:summaryCandidateLimit]
		}

		return fmt.Sprintf("%s ran %s%s%s; its output named %d pull requests: %s%s",
			lead, a.call.tool, target(a.call.args), title, len(a.candidates), strings.Join(named, ", "), more)
	default:
		return fmt.Sprintf("%s ran %s%s%s; its output named no pull request URL",
			lead, a.call.tool, target(a.call.args), title)
	}
}

// target renders the literal repo and head a call named, if any.
func target(args prArgs) string {
	var parts []string
	if args.repo != "" {
		parts = append(parts, "repo "+args.repo)
	}
	if args.head != "" {
		parts = append(parts, "head "+args.head)
	}
	if len(parts) == 0 {
		return ""
	}

	return " (" + strings.Join(parts, ", ") + ")"
}

func truncate(s string, limit int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > limit {
		return s[:limit-3] + "..."
	}

	return s
}

// transcriptAnchors finds every PR-creating call in a transcript, resolves the ones
// whose result is present, and returns the ids of those still awaiting one.
//
// A call with no result yet is DEFERRED, not emitted as unresolved: anchors are keyed
// on the tool_use id and INSERT OR IGNORE never updates a row, so an early unresolved
// anchor would freeze for good and the result arriving later could never correct it
// (F21's mechanism). Claude Code writes a tool_result for every call — rejections and
// interrupts included; all 244 measured PR-creating calls had one — so deferral costs
// only the in-flight window. The waiting ids are returned so the caller records them
// on the snapshot's segment events: a transcript that ends without the result keeps a
// durable trace rather than losing the call silently.
//
// Calls are excluded by the cwd in force on their own line, the rule segments use.
func transcriptAnchors(lines []map[string]any, excludeCwds []string) (resolved []prAnchor, awaiting []string) {
	var calls []prCreateCall
	cwd := ""

	for _, line := range lines {
		if c, _ := line["cwd"].(string); c != "" {
			cwd = c
		}
		if cwd != "" && isExcluded(cwd, excludeCwds) {
			continue
		}
		calls = append(calls, prCreateCalls(line, cwd)...)
	}

	want := make(map[string]bool, len(calls))
	for _, c := range calls {
		want[c.toolUseID] = true
	}
	results := toolResultsFor(lines, want)

	for _, c := range calls {
		res, ok := results[c.toolUseID]
		if !ok {
			awaiting = append(awaiting, c.toolUseID)

			continue
		}
		resolved = append(resolved, resolvePRAnchor(c, res))
	}

	return resolved, awaiting
}

// marshalStrings stores ids in the []any shape a store round trip produces, as
// events.SetTicketKeys does and for the same reason.
func marshalStrings(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}

	return out
}
