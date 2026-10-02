package pipeline

// shared_probe_metrics_test.go computes the shared-context acceptance metrics
// (docs/superpowers/specs/2026-10-02-shared-context-design.md §9, M1–M4 and the M7
// screen) FROM THE STORE FILE, with raw SQL. Two properties are deliberate:
//
//   - It reads narrative_events directly rather than through the store's accessors.
//     Those accessors are what this slice changed, and an instrument that re-runs the
//     code under test cannot detect a bug in it (design-notes #43).
//   - It tolerates a store with no narrative_events.kind column, reading every link
//     as a member, and uses no API this slice added. That is the BASELINE arm: this
//     file and shared_probe_test.go compile on main as they stand (see the latter's
//     header for the one stub to add), so both arms are measured by the same code.
//
// The probe that drives a pass and prints these is shared_probe_test.go; the
// computation is tested offline in shared_probe_fixture_test.go, against a fixture
// whose answers are known.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the probe reads the store file directly

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// probeKindContext is narrative_events.kind's context value, spelled here rather than
// as store.LinkContext so this file compiles on the baseline arm, which has no kinds.
const probeKindContext = "context"

// probeLink is one narrative_events row joined to its event and narrative.
type probeLink struct {
	seq        int64
	narrative  int64
	eventID    int64
	kind       string // "member" or "context"; "member" on a pre-kind store
	confidence sql.NullFloat64
	source     string
	externalID string
	occurredAt time.Time
	artifacts  map[string]any
	status     string
}

func (l probeLink) str(key string) string {
	s, _ := l.artifacts[key].(string)

	return s
}

// isTranscriptSegment is §9's definition of "transcript evidence": a claude_code
// event that is not a PR-creation anchor. Anchors coalesce with their PR by
// construction, so counting them would make M1 trivially perfect (finding 5).
func (l probeLink) isTranscriptSegment() bool {
	_, anchor := l.artifacts["anchor_kind"]

	return l.source == "claude_code" && !anchor
}

// transcript identifies the transcript an event came from: session plus subagent,
// so F33's duplicated root segments can be counted once.
func (l probeLink) transcript() string {
	return l.str("session_id") + "/" + l.str("agent_id")
}

// loadProbeLinks reads every link. On a store with no kind column every link is a
// member and has no confidence.
func loadProbeLinks(db *sql.DB) ([]probeLink, error) {
	var hasKind int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('narrative_events') WHERE name = 'kind'`,
	).Scan(&hasKind); err != nil {
		return nil, fmt.Errorf("checking for narrative_events.kind: %w", err)
	}

	kindCols := `'member', NULL`
	if hasKind > 0 {
		kindCols = `ne.kind, ne.member_confidence`
	}

	rows, err := db.Query(`SELECT ne.link_seq, ne.narrative_id, ne.event_id, ` + kindCols + `,
	        e.source, e.external_id, e.occurred_at, e.artifacts, n.status
	   FROM narrative_events ne
	   JOIN events e ON e.id = ne.event_id
	   JOIN narratives n ON n.id = ne.narrative_id
	  ORDER BY ne.link_seq`)
	if err != nil {
		return nil, fmt.Errorf("reading links: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []probeLink
	for rows.Next() {
		var (
			l         probeLink
			occurred  string
			artifacts string
		)
		if err := rows.Scan(&l.seq, &l.narrative, &l.eventID, &l.kind, &l.confidence,
			&l.source, &l.externalID, &occurred, &artifacts, &l.status); err != nil {
			return nil, fmt.Errorf("scanning a link: %w", err)
		}
		if l.occurredAt, err = time.Parse(time.RFC3339, occurred); err != nil {
			return nil, fmt.Errorf("parsing occurred_at %q: %w", occurred, err)
		}
		if err := json.Unmarshal([]byte(artifacts), &l.artifacts); err != nil {
			return nil, fmt.Errorf("parsing artifacts of %s/%s: %w", l.source, l.externalID, err)
		}
		out = append(out, l)
	}

	return out, rows.Err()
}

// loadWorklessNarratives returns every non-split narrative holding no member link —
// including one holding no link at all, which loadProbeLinks cannot see. F37's shape:
// an open narrative whose title describes events it does not hold.
func loadWorklessNarratives(db *sql.DB) ([]int64, error) {
	var hasKind int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('narrative_events') WHERE name = 'kind'`,
	).Scan(&hasKind); err != nil {
		return nil, fmt.Errorf("checking for narrative_events.kind: %w", err)
	}

	member := ""
	if hasKind > 0 {
		member = ` AND ne.kind = 'member'`
	}

	rows, err := db.Query(`SELECT n.id FROM narratives n
	  WHERE n.status != '` + store.StatusSplit + `'
	    AND NOT EXISTS (SELECT 1 FROM narrative_events ne WHERE ne.narrative_id = n.id` + member + `)
	  ORDER BY n.id`)
	if err != nil {
		return nil, fmt.Errorf("reading narratives with no member link: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning a narrative id: %w", err)
		}
		out = append(out, id)
	}

	return out, rows.Err()
}

// acceptanceMetrics is one rep's M1–M4, read from the store.
type acceptanceMetrics struct {
	// M1: PR narratives (holding, as a member, a GitHub PR event in the window) and
	// how many also hold a transcript segment — by member links only, and by member or
	// context — plus the distinct transcripts behind each.
	PRNarratives, EvidenceByMember, EvidenceByMemberOrContext int
	TranscriptsByMember, TranscriptsByMemberOrContext         int
	// M2: created PR anchors whose member home is their PR's :opened event's home.
	Anchors, AnchorsCoalesced int
	AnchorProblems            []string
	// M3: PRs whose GitHub events all share one member home.
	PRs, PRsIntact int
	PRProblems     []string
	// M4: sharing structure and invariant violations.
	ContextLinks, SharedEvents, MaxFanOut int
	MemberConfidences                     []float64 // ascending; empty on a pre-kind store
	Violations                            []string
}

// measureAcceptance computes M1–M4 over links for the window; workless is
// loadWorklessNarratives' answer, reported as violations.
func measureAcceptance(links []probeLink, workless []int64, window correlator.TimeRange) acceptanceMetrics {
	var m acceptanceMetrics

	memberHome := map[int64][]int64{} // event -> narratives holding it as a member
	contextOf := map[int64][]int64{}  // event -> narratives holding it as context
	byNarrative := map[int64][]probeLink{}
	for _, l := range links {
		byNarrative[l.narrative] = append(byNarrative[l.narrative], l)
		if l.kind == probeKindContext {
			contextOf[l.eventID] = append(contextOf[l.eventID], l.narrative)

			continue
		}
		memberHome[l.eventID] = append(memberHome[l.eventID], l.narrative)
		if l.confidence.Valid {
			m.MemberConfidences = append(m.MemberConfidences, l.confidence.Float64)
		}
	}
	sort.Float64s(m.MemberConfidences)

	measureM1(&m, byNarrative, window)
	measureM2M3(&m, links, memberHome)

	for eid, ns := range contextOf {
		m.ContextLinks += len(ns)
		m.SharedEvents++
		m.MaxFanOut = max(m.MaxFanOut, len(ns))
		if len(memberHome[eid]) == 0 {
			m.Violations = append(m.Violations, fmt.Sprintf("event %d has context links and no member home", eid))
		}
	}
	for eid, ns := range memberHome {
		if len(ns) > 1 {
			m.Violations = append(m.Violations, fmt.Sprintf("event %d is a member of %v", eid, ns))
		}
	}
	for _, n := range workless {
		m.Violations = append(m.Violations, fmt.Sprintf("open narrative %d holds no member event (F37's shape)", n))
	}
	sort.Strings(m.Violations)

	return m
}

func measureM1(m *acceptanceMetrics, byNarrative map[int64][]probeLink, window correlator.TimeRange) {
	memberTranscripts := map[string]bool{}
	anyTranscripts := map[string]bool{}

	for _, ls := range byNarrative {
		isPR := slices.ContainsFunc(ls, func(l probeLink) bool {
			return l.kind != probeKindContext && l.source == "github" &&
				l.str(events.ArtifactPullRequest) != "" &&
				!l.occurredAt.Before(window.Start) && l.occurredAt.Before(window.End)
		})
		if !isPR || ls[0].status == store.StatusSplit {
			continue
		}
		m.PRNarratives++

		byMember, byAny := false, false
		for _, l := range ls {
			if !l.isTranscriptSegment() {
				continue
			}
			byAny = true
			anyTranscripts[l.transcript()] = true
			if l.kind != probeKindContext {
				byMember = true
				memberTranscripts[l.transcript()] = true
			}
		}
		if byMember {
			m.EvidenceByMember++
		}
		if byAny {
			m.EvidenceByMemberOrContext++
		}
	}

	m.TranscriptsByMember = len(memberTranscripts)
	m.TranscriptsByMemberOrContext = len(anyTranscripts)
}

func measureM2M3(m *acceptanceMetrics, links []probeLink, memberHome map[int64][]int64) {
	home := func(eid int64) int64 {
		if hs := memberHome[eid]; len(hs) == 1 {
			return hs[0]
		}

		return 0
	}

	opened := map[string]int64{} // PR -> its :opened event id
	prHomes := map[string]map[int64]bool{}
	seen := map[int64]bool{}
	for _, l := range links {
		if seen[l.eventID] || l.kind == probeKindContext {
			continue
		}
		seen[l.eventID] = true
		pr := l.str(events.ArtifactPullRequest)
		if l.source != "github" || pr == "" {
			continue
		}
		if strings.HasSuffix(l.externalID, ":opened") {
			opened[pr] = l.eventID
		}
		if prHomes[pr] == nil {
			prHomes[pr] = map[int64]bool{}
		}
		prHomes[pr][home(l.eventID)] = true
	}

	for _, l := range links {
		if l.kind == probeKindContext || l.str("anchor_kind") == "" ||
			l.str("pr_create_outcome") != "created" || l.str(events.ArtifactPullRequest) == "" {
			continue
		}
		pr := l.str(events.ArtifactPullRequest)
		m.Anchors++
		oid, ok := opened[pr]
		switch {
		case !ok:
			m.AnchorProblems = append(m.AnchorProblems, fmt.Sprintf("%s: no linked :opened event", pr))
		case home(oid) == home(l.eventID):
			m.AnchorsCoalesced++
		default:
			m.AnchorProblems = append(m.AnchorProblems,
				fmt.Sprintf("%s: anchor in narrative %d, :opened in %d", pr, home(l.eventID), home(oid)))
		}
	}

	for pr, homes := range prHomes {
		m.PRs++
		if len(homes) == 1 {
			m.PRsIntact++

			continue
		}
		hs := make([]string, 0, len(homes))
		for h := range homes {
			hs = append(hs, fmt.Sprint(h))
		}
		sort.Strings(hs)
		m.PRProblems = append(m.PRProblems, fmt.Sprintf("%s: split across narratives %s", pr, strings.Join(hs, ", ")))
	}
	sort.Strings(m.AnchorProblems)
	sort.Strings(m.PRProblems)
}

// attraction is M7's deterministic screen over a two-pass rep: every pass-2 member
// placement (link_seq above the pass-1 high-water mark) whose own PR or branch
// evidence points at a DIFFERENT pass-1 narrative than the one it joined. It is a
// screen, not the verdict — the spec has a reviewer read the placements — and it is
// arm-independent, so baseline and treatment are compared on the same definition.
//
// Attracted are the subset that joined a narrative B holding a pass-1 context link
// to work whose member home is the narrative the evidence points at: the failure the
// summary rule exists to prevent. Always empty on the baseline arm, which has no
// context links.
type attraction struct {
	Screened  []string
	Attracted []string
}

func measureAttraction(links []probeLink, pass1HighWater int64) attraction {
	// Evidence keys of each pass-1 narrative's member events.
	keysOf := func(l probeLink) []string {
		var ks []string
		if pr := l.str(events.ArtifactPullRequest); pr != "" {
			ks = append(ks, "pr:"+pr)
		}
		if br := l.str(events.ArtifactGitBranch); br != "" && br != "main" && br != "master" {
			ks = append(ks, "branch:"+br)
		}

		return ks
	}

	streamOf := map[string]map[int64]bool{} // evidence key -> pass-1 narratives carrying it
	homeOf := map[int64]int64{}             // event -> pass-1 member home
	backgroundFrom := map[int64]map[int64]bool{}
	for _, l := range links {
		if l.seq > pass1HighWater {
			continue
		}
		if l.kind == probeKindContext {
			continue
		}
		homeOf[l.eventID] = l.narrative
		for _, k := range keysOf(l) {
			if streamOf[k] == nil {
				streamOf[k] = map[int64]bool{}
			}
			streamOf[k][l.narrative] = true
		}
	}
	for _, l := range links {
		if l.seq <= pass1HighWater && l.kind == probeKindContext {
			if backgroundFrom[l.narrative] == nil {
				backgroundFrom[l.narrative] = map[int64]bool{}
			}
			backgroundFrom[l.narrative][homeOf[l.eventID]] = true
		}
	}

	var a attraction
	for _, l := range links {
		if l.seq <= pass1HighWater || l.kind == probeKindContext {
			continue
		}
		pointed := map[int64]bool{}
		for _, k := range keysOf(l) {
			for n := range streamOf[k] {
				pointed[n] = true
			}
		}
		if len(pointed) == 0 || pointed[l.narrative] {
			continue
		}
		line := fmt.Sprintf("%s/%s joined narrative %d; its evidence points at %v",
			l.source, l.externalID, l.narrative, sortedKeys(pointed))
		a.Screened = append(a.Screened, line)
		for src := range pointed {
			if backgroundFrom[l.narrative][src] {
				a.Attracted = append(a.Attracted, line)

				break
			}
		}
	}

	return a
}

func sortedKeys(m map[int64]bool) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)

	return out
}

// renderAcceptance prints one rep's metrics in the order §9's table lists them.
func renderAcceptance(m acceptanceMetrics) string {
	var b strings.Builder
	fmt.Fprintf(&b, "M1  PR narratives with transcript evidence: member-only %d/%d, member-or-context %d/%d "+
		"(distinct transcripts %d / %d)\n",
		m.EvidenceByMember, m.PRNarratives, m.EvidenceByMemberOrContext, m.PRNarratives,
		m.TranscriptsByMember, m.TranscriptsByMemberOrContext)
	fmt.Fprintf(&b, "M2  anchor↔PR coalescing: %d/%d\n", m.AnchorsCoalesced, m.Anchors)
	for _, p := range m.AnchorProblems {
		fmt.Fprintf(&b, "      %s\n", p)
	}
	fmt.Fprintf(&b, "M3  PR integrity: %d/%d\n", m.PRsIntact, m.PRs)
	for _, p := range m.PRProblems {
		fmt.Fprintf(&b, "      %s\n", p)
	}
	fmt.Fprintf(&b, "M4  context links %d, shared events %d, max fan-out %d, violations %d\n",
		m.ContextLinks, m.SharedEvents, m.MaxFanOut, len(m.Violations))
	if n := len(m.MemberConfidences); n > 0 {
		q := func(p float64) float64 { return m.MemberConfidences[int(p*float64(n-1))] }
		fmt.Fprintf(&b, "      member confidence min %.2f q1 %.2f median %.2f q3 %.2f max %.2f (n=%d)\n",
			q(0), q(0.25), q(0.5), q(0.75), q(1), n)
	}
	for _, v := range m.Violations {
		fmt.Fprintf(&b, "      VIOLATION %s\n", v)
	}

	return b.String()
}
