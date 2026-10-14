package catalogue

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ticketslinger/ticketslinger/api/internal/audit"
)

// Quota is a capacity shared by the ticket types it lists.
type Quota struct {
	ID            uuid.UUID   `json:"id"`
	EventID       uuid.UUID   `json:"event_id"`
	Name          string      `json:"name"`
	Size          int         `json:"size"`
	TicketTypeIDs []uuid.UUID `json:"ticket_type_ids"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

type QuotaInput struct {
	Name          string
	Size          int
	TicketTypeIDs []uuid.UUID
}

// QuotaPatch changes only what is set. A non-nil TicketTypeIDs replaces the
// whole set (empty is allowed); nil leaves the links alone.
type QuotaPatch struct {
	Name          *string
	Size          *int
	TicketTypeIDs *[]uuid.UUID
}

const quotaCols = `id, event_id, name, size, coalesce((select array_agg(ticket_type_id order by ticket_type_id)
	from quota_ticket_types where quota_id = quotas.id), '{}'), created_at, updated_at`

func scanQuota(row pgx.Row) (Quota, error) {
	var q Quota
	err := row.Scan(&q.ID, &q.EventID, &q.Name, &q.Size, &q.TicketTypeIDs, &q.CreatedAt, &q.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Quota{}, ErrNotFound
	}
	return q, err
}

// validateQuota holds the rules the CHECK constraints in 00009 back up. It
// trims the name first.
func validateQuota(name *string, size int) error {
	*name = strings.TrimSpace(*name)
	switch {
	case utf8.RuneCountInString(*name) < 1 || utf8.RuneCountInString(*name) > 120:
		return invalid("invalid_name", "Give the quota a name of up to 120 characters.")
	case size < 0 || size > 1000000:
		return invalid("bad_quota_size", "A quota holds from 0 to 1,000,000 tickets.")
	}
	return nil
}

// quotaDBError is dbError for quotas: 23514 is a failed CHECK, 23503 a link
// whose quota or ticket type isn't there (or isn't in this event).
func quotaDBError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23514":
			return ErrInvalid{"invalid_quota", "Something in that quota isn't allowed. Check the name, the size and the ticket types."}
		case "23503":
			return ErrInvalid{"invalid_quota", "One of those ticket types isn't part of this event any more. Reload and try again."}
		}
	}
	return err
}

// dedupe sorts ids and drops repeats.
func dedupe(ids []uuid.UUID) []uuid.UUID {
	ids = slices.Clone(ids)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return slices.Compact(ids)
}

// linkSet dedupes ids, then checks inside the locked transaction that they
// are all ticket types of this event.
func linkSet(ctx context.Context, tx pgx.Tx, event uuid.UUID, ids []uuid.UUID) ([]uuid.UUID, error) {
	ids = dedupe(ids)
	var n int
	if err := tx.QueryRow(ctx, `select count(*) from ticket_types where event_id = $1 and id = any($2)`, event, ids).Scan(&n); err != nil {
		return nil, err
	}
	if n != len(ids) {
		return nil, invalid("unknown_ticket_type", "One of those ticket types isn't part of this event.")
	}
	return ids, nil
}

// link writes every quota-by-ticket-type pair; the ids are already checked.
func link(ctx context.Context, tx pgx.Tx, org, event uuid.UUID, quotas, tts []uuid.UUID) error {
	_, err := tx.Exec(ctx, `insert into quota_ticket_types(organizer_id, event_id, quota_id, ticket_type_id)
		select $1, $2, q, t from unnest($3::uuid[]) q, unnest($4::uuid[]) t`, org, event, quotas, tts)
	return quotaDBError(err)
}

// setLinks replaces a quota's ticket types.
func setLinks(ctx context.Context, tx pgx.Tx, org, event, quota uuid.UUID, ids []uuid.UUID) error {
	if _, err := tx.Exec(ctx, `delete from quota_ticket_types where quota_id = $1`, quota); err != nil {
		return err
	}
	return link(ctx, tx, org, event, []uuid.UUID{quota}, ids)
}

// liveNeedsCapacity refuses to leave a live event with a ticket type in no
// quota. The caller holds the event's row lock.
func liveNeedsCapacity(ctx context.Context, tx pgx.Tx, ev Event) error {
	if ev.Status != "live" {
		return nil
	}
	var n int
	if err := tx.QueryRow(ctx, `select count(*) from ticket_types t where event_id = $1
		and not exists (select 1 from quota_ticket_types where ticket_type_id = t.id)`, ev.ID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrConflict{"live_needs_capacity", "Every ticket on a live event needs a capacity. Move the event back to draft first."}
	}
	return nil
}

// CreateQuota adds a quota and records quota.created by actor. tx must be
// scoped to org.
func CreateQuota(ctx context.Context, tx pgx.Tx, org, event uuid.UUID, actor audit.Actor, in QuotaInput) (Quota, error) {
	if err := validateQuota(&in.Name, in.Size); err != nil {
		return Quota{}, err
	}
	if _, err := lockEvent(ctx, tx, event); err != nil {
		return Quota{}, err
	}
	ids, err := linkSet(ctx, tx, event, in.TicketTypeIDs)
	if err != nil {
		return Quota{}, err
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `insert into quotas(organizer_id, event_id, name, size) values ($1, $2, $3, $4) returning id`,
		org, event, in.Name, in.Size).Scan(&id); err != nil {
		return Quota{}, quotaDBError(err)
	}
	if err := setLinks(ctx, tx, org, event, id, ids); err != nil {
		return Quota{}, err
	}
	q, err := getQuota(ctx, tx, event, id)
	if err != nil {
		return Quota{}, err
	}
	return q, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "quota.created", EntityType: "quota", EntityID: q.ID, After: q})
}

func getQuota(ctx context.Context, tx pgx.Tx, event, id uuid.UUID) (Quota, error) {
	return scanQuota(tx.QueryRow(ctx, `select `+quotaCols+` from quotas where id = $1 and event_id = $2`, id, event))
}

func GetQuota(ctx context.Context, tx pgx.Tx, event, id uuid.UUID) (Quota, error) {
	return getQuota(ctx, tx, event, id)
}

// ListQuotas returns the event's quotas, oldest first.
func ListQuotas(ctx context.Context, tx pgx.Tx, event uuid.UUID) ([]Quota, error) {
	if _, err := GetEvent(ctx, tx, event); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `select `+quotaCols+` from quotas where event_id = $1 order by created_at, id`, event)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Quota, error) { return scanQuota(r) })
}

// UpdateQuota applies patch under the event lock and records quota.updated.
// A patch that changes nothing writes nothing. On a live event it can't leave
// a ticket type without a quota: ErrConflict live_needs_capacity.
func UpdateQuota(ctx context.Context, tx pgx.Tx, org, event, id uuid.UUID, actor audit.Actor, p QuotaPatch) (Quota, error) {
	ev, err := lockEvent(ctx, tx, event)
	if err != nil {
		return Quota{}, err
	}
	before, err := scanQuota(tx.QueryRow(ctx, `select `+quotaCols+` from quotas where id = $1 and event_id = $2 for update`, id, event))
	if err != nil {
		return Quota{}, err
	}
	q := before
	if p.Name != nil {
		q.Name = *p.Name
	}
	if p.Size != nil {
		q.Size = *p.Size
	}
	if err := validateQuota(&q.Name, q.Size); err != nil {
		return Quota{}, err
	}
	if p.TicketTypeIDs != nil {
		if q.TicketTypeIDs, err = linkSet(ctx, tx, event, *p.TicketTypeIDs); err != nil {
			return Quota{}, err
		}
	}
	if q.Name == before.Name && q.Size == before.Size && slices.Equal(q.TicketTypeIDs, before.TicketTypeIDs) {
		return before, nil
	}
	if _, err := tx.Exec(ctx, `update quotas set name = $2, size = $3, updated_at = clock_timestamp() where id = $1`, id, q.Name, q.Size); err != nil {
		return Quota{}, quotaDBError(err)
	}
	if !slices.Equal(q.TicketTypeIDs, before.TicketTypeIDs) {
		if err := setLinks(ctx, tx, org, event, id, q.TicketTypeIDs); err != nil {
			return Quota{}, err
		}
		if err := liveNeedsCapacity(ctx, tx, ev); err != nil {
			return Quota{}, err
		}
	}
	q, err = getQuota(ctx, tx, event, id)
	if err != nil {
		return Quota{}, err
	}
	return q, audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "quota.updated", EntityType: "quota", EntityID: id, Before: before, After: q})
}

// DeleteQuota removes a quota and records quota.deleted. On a live event it
// can't strand a ticket type: ErrConflict live_needs_capacity.
func DeleteQuota(ctx context.Context, tx pgx.Tx, org, event, id uuid.UUID, actor audit.Actor) error {
	ev, err := lockEvent(ctx, tx, event)
	if err != nil {
		return err
	}
	before, err := scanQuota(tx.QueryRow(ctx, `select `+quotaCols+` from quotas where id = $1 and event_id = $2 for update`, id, event))
	if err != nil {
		return err
	}
	// ponytail: P3 must guard this once tickets are sold against the quota.
	if _, err := tx.Exec(ctx, `delete from quotas where id = $1`, id); err != nil {
		return err
	}
	if err := liveNeedsCapacity(ctx, tx, ev); err != nil {
		return err
	}
	return audit.Write(ctx, tx, audit.Entry{Organizer: org, Actor: actor, Action: "quota.deleted", EntityType: "quota", EntityID: id, Before: before})
}
