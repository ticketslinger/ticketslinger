// Package audit writes the append-only audit log.
package audit

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Actor is who did it: a user or an API key, never both.
type Actor struct{ User, Key uuid.UUID }

type Entry struct {
	Organizer     uuid.UUID
	Actor         Actor
	Action        string
	EntityType    string
	EntityID      uuid.UUID
	Before, After any // nil is stored as NULL
}

// Write records e in tx. The transaction must be scoped to e.Organizer, or
// row-level security rejects the insert.
func Write(ctx context.Context, tx pgx.Tx, e Entry) error {
	if (e.Actor.User == uuid.Nil) == (e.Actor.Key == uuid.Nil) {
		return errors.New("audit: an entry has exactly one actor, a user or a key")
	}
	before, err := marshal(e.Before)
	if err != nil {
		return err
	}
	after, err := marshal(e.After)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log(organizer_id, actor_user_id, actor_api_key_id, action, entity_type, entity_id, before, after)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		e.Organizer, nullable(e.Actor.User), nullable(e.Actor.Key), e.Action, e.EntityType, e.EntityID, before, after)
	return err
}

func nullable(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func marshal(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}
