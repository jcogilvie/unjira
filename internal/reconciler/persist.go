package reconciler

import (
	"encoding/json"
	"fmt"

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
// case), not via a foreign-key violation: internal/store never issues
// `PRAGMA foreign_keys = ON`, and modernc.org/sqlite defaults FK enforcement
// off, so an insert naming a nonexistent narrative_id would silently succeed
// rather than fail the transaction. See that test's comment for the full
// reasoning.
func Persist(s *store.Store, results []ReconcileResult) ([]store.ActionRow, error) {
	var persisted []store.ActionRow

	err := s.WithTx(func(tx *store.Tx) error {
		for _, result := range results {
			for _, action := range result.Proposed {
				payload, err := actionPayload(action)
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
					Status:      "proposed",
				}

				id, err := tx.InsertAction(row)
				if err != nil {
					return err
				}

				row.ID = id
				persisted = append(persisted, row)
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

// actionPayload encodes the type-specific half of an action as JSON, since
// actions.payload's shape depends on actions.type.
//
// Encoded via a map rather than string concatenation so a body containing
// quotes or newlines cannot corrupt the column — comment bodies are free prose
// and routinely contain both.
func actionPayload(action ProposedAction) (string, error) {
	var payload map[string]any

	switch action.Type {
	case ActionComment:
		payload = map[string]any{"body": action.Body}
	case ActionTransition:
		payload = map[string]any{"target_status": string(action.TargetStatus)}
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
