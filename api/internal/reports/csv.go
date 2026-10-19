package reports

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ticketslinger/ticketslinger/api/internal/catalogue"
)

// ErrUnknownKind is an export kind other than orders.csv, attendees.csv and
// checkins.csv.
var ErrUnknownKind = errors.New("reports: unknown export")

var writers = map[string]func(ctx context.Context, tx pgx.Tx, event uuid.UUID, zone *time.Location, cw *csv.Writer) error{
	"orders.csv":    writeOrders,
	"attendees.csv": writeAttendees,
	"checkins.csv":  writeCheckins,
}

// WriteCSV writes event's export kind to w: a UTF-8 BOM (so Excel reads it
// as UTF-8), then CRLF rows with every cell defused. Times are in the event's
// time zone with their offset, money in dollars like 25.00, nulls empty. It
// never reads a ticket's secret, an order's token or a QR value. The file
// name is "<event-slug>-<kind>", which holds only [a-z0-9.-].
//
// ponytail: the caller builds the file in memory, so a failure midway is
// still an error status; stream rows if an event passes ~50,000 tickets.
func WriteCSV(ctx context.Context, tx pgx.Tx, event uuid.UUID, kind string, w io.Writer) (filename string, err error) {
	write, ok := writers[kind]
	if !ok {
		return "", ErrUnknownKind
	}
	ev, err := catalogue.GetEvent(ctx, tx, event)
	if err != nil {
		return "", err
	}
	zone, err := time.LoadLocation(ev.Timezone)
	if err != nil {
		return "", err
	}
	if _, err := io.WriteString(w, "\xEF\xBB\xBF"); err != nil {
		return "", err
	}
	cw := csv.NewWriter(w)
	cw.UseCRLF = true
	if err := write(ctx, tx, event, zone, cw); err != nil {
		return "", err
	}
	cw.Flush()
	return ev.Slug + "-" + kind, cw.Error()
}

// defuse keeps a spreadsheet from running a cell as a formula: one that
// starts with =, +, -, @, tab or CR gets a leading quote.
func defuse(s string) string {
	if s != "" {
		switch s[0] {
		case '=', '+', '-', '@', '\t', '\r':
			return "'" + s
		}
	}
	return s
}

func row(cw *csv.Writer, cells ...string) error {
	for i, c := range cells {
		cells[i] = defuse(c)
	}
	return cw.Write(cells)
}

func money(c int64) string { return fmt.Sprintf("%d.%02d", c/100, c%100) }

func when(t *time.Time, zone *time.Location) string {
	if t == nil {
		return ""
	}
	return t.In(zone).Format(time.RFC3339)
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// writeOrders lists the orders the order list shows, newest first. Tickets
// bought counts every ticket the order bought; Fee paid by says whether the
// buyer paid the service fee on top or it came out of the organizer's share
// (then Total doesn't include it); Refunded adds up every refund that hasn't
// failed.
func writeOrders(ctx context.Context, tx pgx.Tx, event uuid.UUID, zone *time.Location, cw *csv.Writer) error {
	if err := row(cw, "Order", "Status", "Email", "Tickets bought", "Subtotal", "Discount", "Tax", "Service fee", "Fee paid by", "Total", "Refunded",
		"Promo code", "Placed", "Paid"); err != nil {
		return err
	}
	var code, status, email, payer, promo string
	var tickets int
	var subtotal, discount, tax, fee, total, refunded int64
	var placed time.Time
	var paid *time.Time
	rows, _ := tx.Query(ctx, `select o.code, o.status, o.email::text, (select count(*) from tickets t where t.order_id = o.id),
			o.subtotal_cents, o.discount_cents, o.tax_cents, o.fee_cents, case o.fee_mode when 'pass' then 'buyer' else 'you' end, o.total_cents,
			coalesce((select sum(r.amount_cents) from refunds r join payments p on p.id = r.payment_id where p.order_id = o.id and r.status <> 'failed'), 0)::bigint,
			coalesce(pc.code, ''), o.created_at, o.paid_at
		from (`+listed+`) o left join promo_codes pc on pc.id = o.promo_code_id
		order by o.created_at desc, o.id desc`, event)
	_, err := pgx.ForEachRow(rows, []any{&code, &status, &email, &tickets, &subtotal, &discount, &tax, &fee, &payer, &total, &refunded, &promo, &placed, &paid}, func() error {
		return row(cw, code, status, email, strconv.Itoa(tickets), money(subtotal), money(discount), money(tax), money(fee), payer, money(total),
			money(refunded), promo, when(&placed, zone), when(paid, zone))
	})
	return err
}

// writeAttendees is one row per ticket, any status, then a column per
// question in position order: ticket-scope answers on their ticket,
// order-scope answers repeated on each ticket of the order.
func writeAttendees(ctx context.Context, tx pgx.Tx, event uuid.UUID, zone *time.Location, cw *csv.Writer) error {
	header := []string{"Order", "Ticket", "Ticket type", "Attendee", "Email", "Status", "Checked in"}
	var questions []uuid.UUID
	var q uuid.UUID
	var label string
	rows, _ := tx.Query(ctx, `select id, label from questions where event_id = $1 order by position, id`, event)
	if _, err := pgx.ForEachRow(rows, []any{&q, &label}, func() error {
		questions, header = append(questions, q), append(header, label)
		return nil
	}); err != nil {
		return err
	}
	if err := row(cw, header...); err != nil {
		return err
	}
	// Keyed by the ticket for a ticket-scope answer, by the order otherwise.
	type key struct{ of, question uuid.UUID }
	answers := map[key]string{}
	var of uuid.UUID
	var value string
	rows, _ = tx.Query(ctx, `select coalesce(ticket_id, order_id), question_id, value from answers where event_id = $1`, event)
	if _, err := pgx.ForEachRow(rows, []any{&of, &q, &value}, func() error {
		answers[key{of, q}] = value
		return nil
	}); err != nil {
		return err
	}
	var ticket, order uuid.UUID
	var code, typeName, attendee, email, status string
	var checkedIn *time.Time
	rows, _ = tx.Query(ctx, `select t.id, t.order_id, o.code, tt.name, t.attendee_name, o.email::text, t.status, t.checked_in_at
		from tickets t join orders o on o.id = t.order_id join ticket_types tt on tt.id = t.ticket_type_id
		where t.event_id = $1 order by o.created_at, o.id, t.id`, event)
	_, err := pgx.ForEachRow(rows, []any{&ticket, &order, &code, &typeName, &attendee, &email, &status, &checkedIn}, func() error {
		cells := []string{code, ticket.String(), typeName, attendee, email, status, when(checkedIn, zone)}
		for _, q := range questions {
			a, ok := answers[key{ticket, q}]
			if !ok {
				a = answers[key{order, q}]
			}
			cells = append(cells, a)
		}
		return row(cw, cells...)
	})
	return err
}

// writeCheckins is every scan and manual check-in, oldest first. By is the
// member's email, the key's prefix, or "Team member" where the directory
// won't name them: a former member, or any member when an API key exports
// (the directory answers only signed-in owners and admins, so keys never get
// members' emails).
func writeCheckins(ctx context.Context, tx pgx.Tx, event uuid.UUID, zone *time.Location, cw *csv.Writer) error {
	if err := row(cw, "Time", "Result", "Method", "Ticket", "Ticket type", "Attendee", "Order", "Gate", "By"); err != nil {
		return err
	}
	var at time.Time
	var result, method string
	var ticket *uuid.UUID
	var typeName, attendee, code, gate, by *string
	rows, _ := tx.Query(ctx, `select c.at, c.result, c.method, c.ticket_id, tt.name, t.attendee_name, o.code, c.gate,
			coalesce(d.email::text, 'API key ' || k.prefix, case when c.actor_user_id is not null then 'Team member' end)
		from checkins c
		left join tickets t on t.id = c.ticket_id
		left join ticket_types tt on tt.id = t.ticket_type_id
		left join orders o on o.id = t.order_id
		left join app_member_directory() d on d.user_id = c.actor_user_id
		left join api_keys k on k.id = c.actor_api_key_id
		where c.event_id = $1 order by c.at, c.id`, event)
	_, err := pgx.ForEachRow(rows, []any{&at, &result, &method, &ticket, &typeName, &attendee, &code, &gate, &by}, func() error {
		id := ""
		if ticket != nil {
			id = ticket.String()
		}
		return row(cw, when(&at, zone), result, method, id, str(typeName), str(attendee), str(code), str(gate), str(by))
	})
	return err
}
