package reconciler

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jcogilvie/unjira/internal/store"
)

// Persist writes every proposed action from a whole pass, atomically, and
// returns exactly the rows it wrote (with their assigned ids).
//
// All-or-nothing per pass, in one store.WithTx, because slice 5's auto-commit
// gate requires it (the phase-1 spec: "if reconciliation fails partway through
// a pass, nothing from that pass auto-commits until a clean pass succeeds").
// Half-written proposals would let that gate auto-apply against an
// inconsistent set.
//
// The return value is that same gate's other seam: it identifies "freshly
// proposed this pass" precisely, by construction, rather than by re-deriving
// it from store.ActionsByStatus("proposed") — which returns every action ever
// left at that status, including ones from earlier passes a human has not
// yet triaged. Auto-committing those would violate the gate's own contract
// (only THIS pass's proposals are eligible), so the caller (watch) is handed
// the exact set rather than a status filter it would have to narrow itself.
//
// Nothing here is applied to a tracker: every row lands with status=proposed.
//
// TestPersistWritesNothingWhenOneActionFails proves the rollback by forcing
// an in-loop error via an unrecognized ActionType (actionPayload's default
// case), not via a foreign-key violation. That test predates
// internal/store enforcing foreign keys (store.sqliteDSN now sets
// `_pragma=foreign_keys(1)` in the DSN — see its doc comment); it forces the
// failure this other way to exercise Persist's own error path in isolation,
// decoupled from schema-level FK behavior. See that test's comment for the
// full reasoning.
func Persist(s *store.Store, results []ReconcileResult) ([]store.ActionRow, error) {
	var persisted []store.ActionRow

	err := s.WithTx(func(tx *store.Tx) error {
		for _, result := range results {
			for _, action := range result.Proposed {
				payload, err := ActionPayload(action)
				if err != nil {
					return fmt.Errorf(
						"encoding payload for %s action on narrative %d: %w",
						action.Type, result.NarrativeID, err,
					)
				}

				row := store.ActionRow{
					NarrativeID: result.NarrativeID,
					Type:        string(action.Type),
					IssueKey:    action.IssueKey,
					Payload:     payload,
					Confidence:  action.Confidence,
					Rationale:   action.Rationale,
					Status:      StatusProposed,
				}

				id, err := tx.InsertAction(row)
				if err != nil {
					return err
				}

				row.ID = id
				persisted = append(persisted, row)
			}

			if err := recordSuppression(tx, result); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		// A rolled-back pass persisted nothing; returning the accumulated
		// slice here would hand the caller ids that no longer exist in the
		// table (or, worse, that a future insert reuses), which is exactly
		// the "usable-looking partial result" trap this whole seam exists to
		// avoid. See docs/superpowers/specs/2026-08-26-watch-autocommit-design.md.
		return nil, err
	}

	return persisted, nil
}

// ActionPayload encodes the type-specific half of an action as JSON, since
// actions.payload's shape depends on actions.type.
//
// Encoded via a map rather than string concatenation so a body containing
// quotes or newlines cannot corrupt the column — comment bodies are free prose
// and routinely contain both.
//
// Exported because internal/triage writes replacement action rows directly when
// a reviewer edits or retargets one, and gate.Applier decodes with typed structs
// mirroring this exact shape. A second encoder in triage would be a silent
// divergence: a payload triage wrote that the applier could not read, discovered
// only when a reviewer approved it.
func ActionPayload(action ProposedAction) (string, error) {
	var payload map[string]any

	switch action.Type {
	case ActionComment:
		payload = map[string]any{"body": action.Body}
	case ActionTransition:
		payload = map[string]any{"target_status": action.TargetStatus}
		if len(action.Route) > 1 {
			// Only for a genuine multi-hop route. A one-element route carries
			// no information the target does not already, and omitting it keeps
			// the common payload identical to what shipped before multi-hop —
			// so an action persisted by an older build stays readable.
			payload["route"] = action.Route
		}
	case ActionCreate:
		payload = map[string]any{"summary": action.Summary, "description": action.Body}
	default:
		return "", fmt.Errorf("unknown action type %q", action.Type)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	return string(encoded), nil
}

// recordSuppression writes one watermark row for a narrative a pass examined and
// suppressed, so the same suppression is not re-derived on every future pass.
//
// This is the second half of finding F12, and the create path had already solved
// the identical problem — see StatusDeclined's doc comment, which records the same
// three-pass measurement. Suppression used to write nothing, so DeltaEvents kept
// returning the same links and Reconcile's stable ORDER BY (window_start, id)
// LIMIT 20 re-examined the identical 20 narratives forever, at full model cost,
// while the 35 behind them in the order were never reached.
//
// ONE row per narrative per pass, not one per reason. The row's only job is to
// advance the watermark; the reasons ride along in the rationale so the decision
// stays auditable, which per-reason rows would not improve. IssueKey is left empty
// deliberately: a pass can suppress drafts against several issues, and picking one
// would imply this row is about that issue. Payload is a JSON object rather than
// empty text because the column is NOT NULL and every reader (bodyOf, ActionPayload)
// expects decodable JSON.
//
// Not appended to `persisted`: that slice is what the auto-commit path may apply,
// and gate.Applier handed a row with no payload to post would fail on it.
func recordSuppression(tx *store.Tx, result ReconcileResult) error {
	if len(result.Suppressed) == 0 {
		return nil
	}

	if _, err := tx.InsertAction(store.ActionRow{
		NarrativeID: result.NarrativeID,
		Type:        string(ActionComment),
		Payload:     "{}",
		Rationale:   strings.Join(result.Suppressed, "; "),
		Status:      store.StatusSuppressed,
	}); err != nil {
		return fmt.Errorf(
			"recording suppression for narrative %d: %w", result.NarrativeID, err)
	}

	return nil
}
