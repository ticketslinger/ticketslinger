// Package audit writes the append-only audit log.
package audit

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Actor is who did it: a user, an API key, or the system itself acting
// alone (a late payment refunded). Exactly one of the three.
type Actor struct {
	User, Key uuid.UUID
	System    bool
}

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
	n := 0
	for _, set := range []bool{e.Actor.User != uuid.Nil, e.Actor.Key != uuid.Nil, e.Actor.System} {
		if set {
			n++
		}
	}
	if n != 1 {
		return errors.New("audit: an entry has exactly one actor: a user, a key or the system")
	}
	before, err := marshal(e.Before)
	if err != nil {
		return err
	}
	after, err := marshal(e.After)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log(organizer_id, actor_user_id, actor_api_key_id, actor_system, action, entity_type, entity_id, before, after)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.Organizer, nullable(e.Actor.User), nullable(e.Actor.Key), e.Actor.System, e.Action, e.EntityType, e.EntityID, before, after)
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
