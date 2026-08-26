package reconciler

import (
	"encoding/json"
	"fmt"

	"github.com/jcogilvie/unjira/internal/store"
)

// Persist writes every proposed action from a whole pass, atomically.
//
// All-or-nothing per pass, in one store.WithTx, because slice 5's auto-commit
// gate requires it (the phase-1 spec: "if reconciliation fails partway through
// a pass, nothing from that pass auto-commits until a clean pass succeeds").
// Half-written proposals would let that gate auto-apply against an
// inconsistent set.
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
func Persist(s *store.Store, results []ReconcileResult) error {
	return s.WithTx(func(tx *store.Tx) error {
		for _, result := range results {
			for _, action := range result.Proposed {
				payload, err := actionPayload(action)
				if err != nil {
					return fmt.Errorf(
						"encoding payload for %s action on narrative %d: %w",
						action.Type, result.NarrativeID, err,
					)
				}

				if _, err := tx.InsertAction(store.ActionRow{
					NarrativeID: result.NarrativeID,
					Type:        string(action.Type),
					IssueKey:    action.IssueKey,
					Payload:     payload,
					Confidence:  action.Confidence,
					Rationale:   action.Rationale,
					Status:      "proposed",
				}); err != nil {
					return err
				}
			}
		}

		return nil
	})
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
