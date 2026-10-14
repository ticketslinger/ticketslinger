// Package audit writes the append-only audit log.
package audit

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Entry struct {
	Organizer, Actor uuid.UUID
	Action           string
	EntityType       string
	EntityID         uuid.UUID
	Before, After    any // nil is stored as NULL
}

// Write records e in tx. The transaction must be scoped to e.Organizer, or
// row-level security rejects the insert.
func Write(ctx context.Context, tx pgx.Tx, e Entry) error {
	before, err := marshal(e.Before)
	if err != nil {
		return err
	}
	after, err := marshal(e.After)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log(organizer_id, actor_user_id, action, entity_type, entity_id, before, after)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		e.Organizer, e.Actor, e.Action, e.EntityType, e.EntityID, before, after)
	return err
}

func marshal(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}
