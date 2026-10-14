package catalogue

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// salesExist takes the lock every sale takes (the quotas of the ticket types,
// in id order; all the event's quotas when ticketTypes is nil) and then says
// whether any ticket was ever cut for them or an open checkout holds them now.
// Taking the same lock first is what stops a checkout and an edit from
// passing each other.
func salesExist(ctx context.Context, tx pgx.Tx, event uuid.UUID, ticketTypes []uuid.UUID) (bool, error) {
	if _, err := tx.Exec(ctx, `select 1 from quotas where event_id = $1
		and ($2::uuid[] is null or id in (select quota_id from quota_ticket_types where ticket_type_id = any($2)))
		order by id for update`, event, ticketTypes); err != nil {
		return false, err
	}
	var sold bool
	err := tx.QueryRow(ctx, `select exists (select 1 from tickets where event_id = $1 and ($2::uuid[] is null or ticket_type_id = any($2)))
		or exists (select 1 from holds h join checkouts c on c.id = h.checkout_id
		            where h.event_id = $1 and c.status in ('open', 'paying') and c.expires_at > now()
		              and ($2::uuid[] is null or h.ticket_type_id = any($2)))`, event, ticketTypes).Scan(&sold)
	return sold, err
}

// errHasSales is the 409 for an edit that would change what people have
// bought or are buying. field is the property refused, "" for a delete.
func errHasSales(field, what string) error {
	return ErrConflict{"has_sales", field, "People have already bought or are buying " + what + ", so this can't change now."}
}

// guardSales returns errHasSales when salesExist says so.
func guardSales(ctx context.Context, tx pgx.Tx, event uuid.UUID, ticketTypes []uuid.UUID, field, what string) error {
	sold, err := salesExist(ctx, tx, event, ticketTypes)
	if err != nil {
		return err
	}
	if sold {
		return errHasSales(field, what)
	}
	return nil
}
