package catalogue

import (
	"context"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
)

// reorder renumbers table's rows for the event 1..n in the order of ids under
// the event lock. table is one of our own names, never input. ids that aren't
// exactly the current set are 422 order_mismatch; the current order again is
// a no-op with no write and no audit entry.
func reorder(ctx context.Context, tx pgx.Tx, org, event uuid.UUID, actor audit.Actor, table, action string, ids []uuid.UUID) error {
	if _, err := lockEvent(ctx, tx, event); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `select id from `+table+` where event_id = $1 order by position, created_at, id`, event)
	if err != nil {
		return err
	}
	current, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	// Equal length and every current id present once: the same set.
	sorted := func(s []uuid.UUID) []uuid.UUID {
		s = slices.Clone(s)
		slices.SortFunc(s, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
		return s
	}
	if !slices.Equal(sorted(ids), sorted(current)) {
		return invalid("order_mismatch", "Send every id in this event exactly once. The list is out of date, has a repeat, or has one that isn't here. Reload and try again.")
	}
	if slices.Equal(ids, current) {
		return nil
	}
	if _, err := tx.Exec(ctx, `update `+table+` t set position = o.n from unnest($1::uuid[]) with ordinality as o(id, n) where t.id = o.id`, ids); err != nil {
		return err
	}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: action, EntityType: "event", EntityID: event,
		Before: map[string][]uuid.UUID{"ids": current}, After: map[string][]uuid.UUID{"ids": ids}})
}
